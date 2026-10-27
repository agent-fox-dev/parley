# Port the af-hub service to TypeScript on Cloudflare

## Intent

Teams that run coding agents need one service that holds the repository, the
credentials, the sandbox and the record of what an agent did. That service
exists today as `af-hub`, a Go binary that expects a Linux host, a POSIX
filesystem and a `git` executable. Parley is the same service for a serverless
deployment: the same HTTP contract, the same `afc` client, no host to operate.
Keeping the contract identical is what makes the move cheap — every agent,
script and CI job that speaks to a hub keeps working — and keeping the
platform behind an interface is what stops the move from trading one lock-in
for another.

## Goals

- **G1 — API compatibility.** Every route, status code, `error_type`, response
  field and ordering rule described in `docs/prd/api.md` behaves as that
  document specifies. A conformance suite asserts this route by route and is
  part of the project's checks.
- **G2 — CLI compatibility.** The `afc` binary built from
  `github.com/agent-fox-dev/hub`, unmodified, runs every command in
  `docs/prd/cli.md` against a Parley deployment and produces the exit codes
  and stdout documents that document specifies. The Quick Tour of
  `docs/prd/cli.md` (login, workspace create --wait, credential helper, git
  clone, git push, merge submit --wait, workspace push) runs end to end with
  exit code 0 at every step.
- **G3 — Web UI.** A browser reaches a landing page, signs in with the same
  OAuth provider the API uses, and sees a read-only dashboard, in the visual
  language of GitHub and GitHub Copilot, that lists the signed-in user's
  workspaces and shows one workspace's state (status, head, patches where it
  is `carry_patch`, recent jobs, recent audit events).
- **G6 — Proved history.** Every rebase and merge Parley computes produces the
  same history as the real `git` binary on the same inputs, asserted over a
  corpus of at least 200 generated cases as part of the project's checks.
- **G4 — Portability.** Every use of a Cloudflare product sits behind a
  named interface in the codebase. The full test suite runs against a second,
  non-Cloudflare implementation of those interfaces, so a Kubernetes
  deployment needs new adapters and no change to the service code.
- **G5 — Operability.** A deployment is created from a checked-in
  configuration with one command, and `GET /healthz`, `GET /readyz` and
  `GET /version` answer as `docs/prd/api.md` specifies.

## Non-goals

- **Changing the contract.** No new routes, fields, status codes or error
  types, and no removal of existing ones, except the divergences this document
  records under Design Decisions. Improvements to the API belong in a later
  PRD against Parley, not in the port.
- **The sandbox gateway.** The hub's AgentKit gRPC gateway, the container
  provider and sandbox profiles are not implemented. The lease and profile
  routes are served in the shape `docs/prd/api.md` requires of a hub with no
  `[sandbox]` section: `404`, `error_type: sandboxes_disabled`, to every
  authenticated caller before any scope check. Sandboxes on Cloudflare are a
  later PRD (decision D4).
- **Porting the `afc` CLI.** `afc` stays the Go binary from the hub
  repository; it is Parley's client and its conformance witness, not part of
  this work.
- **Porting the hub's Go source.** Parley is written against the API and CLI
  references, not transliterated from Go. The hub repository is read for
  behaviour the references leave open, and each such reading is recorded.
- **A migration path from an existing hub deployment.** No importer for a
  running hub's database, repositories or secrets. A later PRD if anyone
  needs one.
- **Windows.** Development runs on Linux and macOS, as `.specs/steering.md`
  requires.
- **Multi-region or multi-account deployment topologies**, and any
  availability or latency target beyond the timeouts the API reference already
  names (30 s for a forge call, 2 min for a branch push).

## Background

**The repository is empty.** `git ls-files` lists `README.md`, `LICENSE`,
`AGENTS.md`, `.specs/steering.md` and three documents under `docs/prd/`:
`start-here.md` (the request), `api.md` and `cli.md`. There is no source, no
package manifest, no build and no spec package (`speca --list --all` reports
0 packages). Everything below is new.

**`docs/prd/api.md`** (4,987 lines) is the hub's complete REST contract and is
the normative input for this work: about 130 routes under `/api/v1`, the git
server under `/git/`, the webhook ingress at `/hooks/forge/:slug`, and four
server routes at the root. It fixes the credential kinds (API key, PAT, admin
token, workspace-bound PAT), the 24 permission scopes, the three workspace
access levels with their anti-enumeration rule, the error envelope and its 40
error types, the workspace response schema and its 12 settings, and the 60-odd
`hub.*` audit events.

**`docs/prd/cli.md`** (2,213 lines) is the contract of the `afc` client that
must keep working: its configuration resolution order, its four exit codes,
its `--wait` polling behaviour, the stdout document of every command, and the
git credential helper that lets `git` authenticate to the hub's git server.

**What the hub assumes.** The hub keeps a working clone per workspace on a
local filesystem and shells out to the `git` CLI: clone, fetch, fast-forward,
rebase, merge, rerere, `receive-pack` behind a pre-receive hook, and a
worktree whose checked-out files follow `HEAD`. Cloudflare Workers have no
filesystem, no subprocess and a CPU budget per request.

**Cloudflare Artifacts is the repository store** (decision D2, taken by the
project owner). It is Cloudflare's versioned storage that speaks Git, in open
beta on the Workers Paid plan, billed per operation and per stored byte from
14 October 2026. What it gives the port, and what it does not, shapes four of
the requirement areas below:

| Artifacts provides | Artifacts does not provide |
|---|---|
| A durable repository per workspace, reachable from a Worker binding and over the full git smart HTTP protocol (v1 and v2, shallow clone, have/want negotiation) | Any server-side history operation: no merge, no rebase, no cherry-pick, no rerere |
| Repo-scoped `read`/`write` tokens, minted on demand with a TTL from 60 s to one year | A pre-receive hook, or any way to refuse one ref of a push. The only push signal is the `cf.artifacts.repo.pushed` event, which fires afterwards |
| Control-plane methods on the binding: create, get, list, import, delete, fork, info, token management, and read-only history access (`log`, `readCommit`, `readTree`, `readBlob`, `readFile`) | An object-level write API. Everything is written by pushing over git |
| A one-time `import()` from a **public** HTTPS remote | Credentials on `import()`, or any re-import or ongoing fetch from an external remote |
| Push, clone, fetch, create, import, fork, delete and token events, deliverable to a queue | A documented guarantee about arbitrary ref namespaces; only `refs/notes/*` is named besides branches and tags |
| 1 GB of storage per repository created on or after 29 September 2026 | Anything larger, so a workspace's repository has a hard 1 GB ceiling |

Four consequences follow, and the requirements below are written around them.
**First**, Parley serves the hub's own git URL and relays to Artifacts, because
`hub_url`, HTTP Basic with a *hub* credential, the hub's access rules and push
control are all part of the contract and none of them is something Artifacts
can do. **Second**, every history operation the hub performs — the initial
copy of a private repository, each sync, each rebuild, each merge job, each
batch rebase, each branch push to the forge — is computed by Parley and
written back to Artifacts over git. **Third**, rerere is Parley's own record of
conflict resolutions, not git's.

**Fourth**, that computation is TypeScript running inside the platform's
per-invocation limits (decision D14). Nothing in the deployment shells out to
`git`. Rebase, the two merge strategies and rerere are therefore new code, not
a wrapper, and two things follow that ordinary requirements would not demand:
their output has to be **proved equal to real git's** rather than merely
plausible, and every operation needs a **stated size at which it still
works** and a documented refusal past it, because a Worker that runs out of
CPU or memory would otherwise leave a job stalled.

**The quality gate.** `AGENTS.md` and `.specs/steering.md` describe a Go
project: `go 1.27`, `make check` as `fmt`, `vet`, `lint` and the short tests,
`make doc` writing `go doc` output to `docs/generated/`. Parley is TypeScript,
so those targets need TypeScript equivalents behind the same names; the
`docs/generated/` rule does not apply (decision D11).

## Requirements

Constraints, from `docs/prd/start-here.md` and from the project owner: the
implementation language is TypeScript; the first deployment target is
Cloudflare; the repository store is Cloudflare Artifacts. The other suggested
services — Workers, Pages, Durable Objects, Queues, R2, D1, with React, Vite
and Hono — remain suggestions.

**How to read this section.** `docs/prd/api.md` and `docs/prd/cli.md` are the
normative contract and are in this repository. An area below names the part of
that contract it covers and states only what those documents do not: the
behaviour the port adds, the behaviour it changes, and the failures that are
new because the platform is new. Where an area says "as `api.md` specifies",
every status, `error_type`, field name, ordering rule and message in the named
sections of that document is a requirement of this work.

### A. Service skeleton and request contract

- The service answers at a configured base URL. Routes live under the mount
  point `/api/v1`; `GET /healthz`, `GET /readyz`, `GET /version`,
  `GET /metrics`, `/git/...` and `POST /hooks/forge/:slug` are served at the
  root, as `api.md` § Conventions and § Server Endpoints specify.
- Every error is the apikit envelope `{"error": {"code", "message",
  "error_type"?}}`, with `error_type` present exactly where `api.md`
  § Error types lists one and omitted otherwise.
- A request body larger than the configured maximum is `413` before any route
  sees it. Timestamps are RFC 3339, rendered in UTC; a timestamp a client
  sends may carry an offset and is normalised to UTC. Cursor pagination
  behaves as § Conventions specifies, with the per-route defaults and caps.
- `GET /healthz` answers `200 {"status":"ok"}` whenever the Worker runs.
  `GET /readyz` answers `200 {"status":"ready"}` when the database answers and
  `503` with the error envelope when it does not.
- `GET /version` answers `build_time`, `commit`, `mount_point` and
  `runtime_version` — the JavaScript runtime's version, under a truthful key
  in place of the hub's `go_version` (decision D8). It is cached publicly.
- `GET /metrics` answers Prometheus text format with the metric names, types
  and labels `api.md` § GET /metrics lists, and the route-template `path`
  label rather than the requested URI. A metric the port has no source for
  reports 0 rather than being omitted, as `api.md` already specifies for
  `afhub_jobqueue_depth`.
- A path under the mount point that matches no route is `404`; a bound token
  on such a path is `403 token_bound`, as § Workspace-Bound Tokens specifies.

### B. Identity, credentials and access

Covers `api.md` § Authentication, § Permission Scopes, § Implied Permissions,
§ Workspace Access, § Anti-Enumeration Policy, § Workspace-Bound Tokens and
§ Identity and Access Endpoints.

- Three credential kinds are accepted in `Authorization: Bearer`: an API key,
  a personal access token, and an admin token. API keys and admin tokens hold
  every hub scope implicitly; only PATs are checked against scopes, and
  `write` implies `read` for every hub resource. A request without a valid
  credential is `401`. A blocked user is refused by the API and by the git
  server.
- Credential strings keep the hub's shapes, because `afc` and the hub's docs
  depend on them: an API key is `af_<key_id>_<secret>`, a PAT is
  `af_pat_<token_id>_<secret>`. Only a hash is stored; no response, log line
  or audit event carries a secret, and the request's `Authorization` header is
  never logged.
- OAuth login works as § Login specifies: `GET /api/v1/auth/providers` is
  public and cached five minutes; `POST /api/v1/auth/callback` exchanges a
  code, creates or updates the user, **revokes every existing API key of that
  user**, issues a new one and gives a new user a personal organization. There
  is no password login.
- Workspace access is decided in one place for every route that names a
  workspace: admin token → `admin`; the owner → `owner`; a member of the
  workspace's organization → `member`; everyone else → none. Membership is
  read on every request. The three levels (read, write, private) answer as the
  § Workspace Access table specifies, including: a member's write is `403
  forbidden`, a stranger is `404 workspace not found`, the access check runs
  after a PAT's scope check and before status and precondition checks, and a
  failed access lookup is `500` and never a grant.
- The anti-enumeration rule holds: `GET /workspaces`, `GET`, `PATCH` and
  `DELETE /workspaces/:slug`, `POST .../archive` and `.../reactivate` answer a
  PAT that lacks the scope with `404`, not `403`; every other route answers
  `403`.
- A workspace-bound PAT behaves exactly as the § Workspace-Bound Tokens table
  specifies, row by row, including the `403 token_bound` message texts, the
  pkt-line `404 repository not found` on the git server, and the
  `hub.token.bound_refused` event at most once per token per minute.
- Deleting a workspace revokes its live bound tokens and removes their
  bindings in one transaction. A periodic job, at start and every hour,
  revokes the live tokens of bindings whose workspace is gone and removes
  bindings whose token is gone; bindings of revoked or expired tokens of an
  existing workspace are kept. A binding that cannot be read is `500`, never a
  grant.
- The apikit-provided identity routes (user, keys, tokens, token permissions,
  orgs, and the administration routes for users and organizations) answer as
  § Identity and Access Endpoints specifies, with apikit's own scopes and the
  `ETag`/`304` behaviour the entries name. The hub's `apikit` Go library has
  no TypeScript equivalent, so these routes are implemented from the
  reference, not wrapped.

### C. Workspaces

Covers `api.md` § Workspace Response Schema, § Workspace Endpoints and
§ Workspace Settings Reference.

- Create, list, read, update, archive, reactivate and delete behave as those
  sections specify: the slug rules, the immutable fields, the `settings`
  object with the effective value of all twelve keys, `hub_url`, the `access`
  field, and the status progression `pending` → `cloning` → `ready` | `failed`
  → `archived`.
- Creation credentials (`git_pat`, or `git_username` and `git_password`) are
  checked against the repository before the workspace is created and stored as
  the workspace secrets `GIT_PAT`, or `GIT_USERNAME` and `GIT_PASSWORD`.
- A workspace holds a lock covering the operations `api.md` § Workspace Lock
  names. A conflicting request answers `409 workspace_busy` at once and may be
  retried; rebuild and merge jobs wait their turn. A carry-patch rebuild
  blocks the others only at its start and end; archive and reclone answer
  `409 workspace_busy` for the whole rebuild. The lock holds across every
  component of the deployment, not only within one of them, and is released if
  the component holding it dies.
- Archiving a `ready` workspace first pushes every branch of its repository to
  `origin` with the stored credentials, then releases the stored repository;
  if that push fails or there are no usable credentials, the workspace is
  archived and the repository is kept, so no commit is lost and reactivation
  reuses it. Active agent sessions are ended as `terminated`, one
  `hub.session.force_closed` each. Bound tokens are left alone.

### D. Repository storage and the git server

Covers `api.md` § Git Server Endpoints in full.

- Each workspace's repository is held in Cloudflare Artifacts, one Artifacts
  repository per workspace, and survives the restart or eviction of every
  other component. Two operations that write one workspace's repository never
  run at the same time (area C's lock).
- A workspace's repository may hold at most 1 GB. A clone, sync or push that
  would exceed it fails with the failure Artifacts reports, surfaced as the
  workspace's `clone_error` or `sync_error` and as the route's documented
  storage-failure answer, and the workspace is left usable.
- Parley serves git smart HTTP at
  `<external_url>/git/<org-slug>/<workspace-slug>.git` for clone, fetch and
  push, and relays to the workspace's Artifacts repository with a repo-scoped
  token that Parley mints and that never reaches the client. A URL without the
  `.git` suffix is `404`; a workspace with no organization has no URL and
  `hub_url` is null. HTTP Basic authentication: the username is ignored, the
  password is a hub credential (API key, PAT or admin token) — never an
  Artifacts token.
- Access and refusals are the § Git Access table: owner and admin token are
  served for both; a member is served for fetch and refused for push with the
  pkt-line `403 ERR org members have read-only access to this workspace`;
  everyone else gets `404 ERR repository not found`. The checks run in the
  documented order (URL match, repository ready, access, then a PAT's scope),
  a failed access lookup is a pkt-line `500`, and a refused push changes
  nothing and records no event.
- `info/refs` serves both services and is `403 ERR invalid or missing service
  parameter` without a valid one. A fetch into a repository that already holds
  part of the history transfers only the missing objects. Both protocol v0 and
  protocol v2 are served, whichever the client asks for (decision D3).
- Push control on a `carry_patch` workspace produces the outcomes the
  § Pre-Receive Hook table specifies for each combination of
  `patch_branch_source` and `push_patches_to_origin` — refused, forwarded to
  the fork first, mirrored afterwards, or accepted. Because the store offers
  no pre-receive hook, Parley decides each ref of a push before any of it
  reaches the store. A push whose refs are partly refused still applies the
  accepted ones and reports each refused ref on its own, with the documented
  message, as `git push` shows it.
- No client push may create, update or delete a ref under
  `refs/hub/replaced/`, on any workspace with any credential; such an update
  is reported `ng <ref> refs/hub/replaced/ is maintained by the hub and cannot
  be pushed to`, the push itself reports `unpack ok`, and the other refs are
  processed as usual. `git ls-remote` and
  `git fetch <hub_url> refs/hub/replaced/<branch>` read those refs normally.
  Whether the store accepts this namespace, a force push and a ref deletion
  is established before anything relies on it, and the fallback is fixed in
  advance (decision D16).
- After a push, the four § After a push effects happen, in order, for the
  accepted refs only: `head_sha` is refreshed, `hub.git.push` lists the
  accepted refs, a rebuild is queued for an accepted patch branch unless
  `auto_rebuild_after_push` is false, and accepted patch branches are mirrored
  to the fork where the settings say so.

### E. Repository lifecycle: clone, sync and branch push

Covers `api.md` § POST .../sync (both modes and the Carry-Patch Sync
Extension), § POST .../reclone and § POST .../branches/:name/push.

- Creating or reactivating a workspace copies the repository at `git_url` into
  the workspace's store, with the workspace's stored credentials where it has
  them, so that **a private repository works** — it is the ordinary case, not
  an edge case. `status` follows the copy (`pending` → `cloning` → `ready` |
  `failed`) and `clone_error` records a failure. Artifacts' own `import()`
  takes no credential and runs once, so it cannot be the only path; see Design
  Decision D15.
- A standard sync fetches `origin` with the stored credentials and produces
  one of the six documented outcomes (up to date, fast-forward, reset on
  `reset_to_upstream`, diverged `409`, fetch failure `502`, cancelled `504`),
  writes `sync_error` on failure and clears it on the next success, and emits
  `hub.workspace.sync` with its `result`.
- A carry-patch sync performs the eight documented steps in order, produces
  the documented response fields (`patches_merged`, `rebuild_triggered`,
  `rebuild_job_id`, `force_push_detected`, `origin_fetched`, `patches_synced`,
  `patches_diverged`), applies `patch_branch_source`,
  `patch_divergence_policy` and `squash_merge_detection`, and reports a step-5
  refresh failure as `500` with the documented messages, leaving
  `upstream_head_sha` and `last_sync_at` unwritten.
- Reclone pushes what it can to `origin`, discards the stored repository and
  copies it again, clearing `sync_error` and `upstream_head_sha`; a failed
  push does not stop it.
- Branch push performs the eight documented checks in that order and answers
  with the documented body and `result` (`pushed`, `up_to_date`, `forced`).
  It runs to its end or its two-minute deadline even if the caller goes away.
  Every attempt records one `hub.branch.push`; refusals and `500`s record
  none. No answer carries a credential, the userinfo of `git_url` or the
  underlying git error.
- **Size.** A workspace whose repository holds at most 500 MB of packed
  objects and at most 10,000 refs is copied, synced and pushed successfully.
  Past that, the operation fails with the route's documented storage or
  server-error answer, naming the limit it exceeded, rather than timing out.
- Work that cannot finish in one unit of platform work continues in another
  and finishes. A copy or sync that fails for any reason leaves the workspace
  in a state a retry recovers from: no operation leaves a workspace in
  `cloning` for longer than 30 minutes, after which it becomes `failed` with
  `clone_error` set.

### F. Carry-patch: patches, rebuilds and rerere

Covers `api.md` § Patch Endpoints, § Rerere Management Endpoints, § Rebuild
Endpoints and § Patch Status Dashboard Endpoint.

- The patch list is ordered, 1-based and contiguous after every add, remove,
  reorder, restore and rebuild. Adds (single and batch) apply the documented
  branch-resolution order (`local`, `origin_tracking`, `origin_fetch`), the
  documented batch validation order, and `skip_branch_check` and
  `if_not_exists`.
- `reset-to-origin` applies its five preconditions in order and the four-case
  action table, keeps a discarded tip under `refs/hub/replaced/<branch>`, and
  reports `replaced_sha`, `rebuild_triggered` and `rebuild_job_id`.
- A rebuild recreates the integration branch from the upstream head plus the
  patches, in position order, by the chosen strategy — `rebase` applies each
  of a patch's own commits, `merge` joins the branch with a merge commit —
  skipping `merged_upstream`, `disabled`, `deleted` and branch-not-found
  patches with the documented `skipped_reason`. The result for every patch
  that applies cleanly is the commit history a `git rebase` or `git merge` of
  the same inputs produces: same trees, same parentage, same order.
- On a conflict the rebuild replays a resolution recorded for that same
  conflict by an earlier rebuild of the workspace, and then honours
  `fail_fast` (the job fails) or `continue` (the patch becomes `conflict`, its
  changes are left out, the rebuild goes on). A resolution recorded by one
  rebuild is replayed by a later one, across restarts of whatever computed it.
  Resolutions are listed with `id`, `path`, `recorded_at` and `resolved`, and
  are forgotten by `id` or, while the conflict is in progress, by path.
- At the end a rebuild sets the integration branch, marks `merged_upstream`
  patches `deleted`, closes the positions, and force-pushes the integration
  branch to `origin` when `rebuild_push_integration_branch` is on (a failed
  push emits `hub.rebuild.push_failed` and does not fail the job).
- A patch tip or upstream head that moves during a rebuild does not change the
  running job; one follow-up rebuild is queued afterwards, `submitted_by`
  `system:stale-snapshot`, event `hub.rebuild.followup`.
- `patch_results` is readable while a job runs, updated as each patch
  finishes, so a client can poll progress. Rebuild submit, list, read, cancel,
  requeue, rollback and preview answer as their entries specify; the preview
  predicts every non-`disabled`, non-`merged_upstream` patch in order without
  writing anything, and a predicted conflict does not stop later predictions.
- The patch-status dashboard answers the documented object, including
  `summary.total_rerere_resolutions` and the two origin-divergence counts.
- **Correctness against git.** For any patch that applies without conflict,
  the commit a rebuild produces has the same tree, the same parents and the
  same position in the history as the commit the real `git` binary produces
  from the same inputs with the same strategy. A conflict is reported for the
  same files git reports it for. This is asserted by comparison against the
  real `git` binary, not by example (area O).
- **Size.** A rebuild applies a stack of at least 50 patches onto a repository
  of at most 500 MB of packed objects, with at most 1,000 changed files and
  10 MB of diff per patch. Past any of those, the job ends `failed` with an
  `error` that names the limit exceeded.
- **Progress.** A rebuild that cannot finish in one unit of platform work
  continues in another. A job that makes no progress for 15 minutes becomes
  `dead_letter` and can be requeued; no job stays `running` indefinitely, and
  no job reports success with a patch stack half applied. The integration
  branch is either the complete result or unchanged.

### G. Merges and batch rebase

Covers `api.md` § Merge Endpoints.

- A merge job rebases `source_ref` onto `target_branch` and fast-forwards the
  target, runs in the background, waits its turn for the workspace lock, and
  moves through `queued` → `running` → `completed` | `failed` | `dead_letter` |
  `cancelled` with `base_sha`, `merged_sha`, `conflict_files`, `error` and
  `retry_count` as the schema specifies. Submit, list, read, cancel and
  requeue answer as their entries specify, including `409 duplicate_merge`.
- Batch rebase runs while the request waits, rebases each branch onto the
  target in turn, and answers one `ok`, `conflict` or `error` result per
  branch; a conflict on one branch does not stop the others.
- A job survives the restart or eviction of whatever component runs it: it is
  either completed or returned to `queued`, never silently lost, and never
  leaves the target branch half-written. A job that makes no progress for 15
  minutes becomes `dead_letter`.
- A merge and a batch rebase produce the history the real `git` binary
  produces from the same inputs, and report a conflict for the same files, as
  in area F. Batch rebase answers within the platform's request limit for a
  target of at most 20 branches; past that it is `400`.

### H. Secrets and variables

Covers `api.md` § Secrets Endpoints and § Variables Endpoints.

- Three scopes each for secrets and variables (user, organization by slug,
  workspace), with the documented access rules — notably that workspace
  secrets are private access while workspace variables are read access to read
  and write access to change.
- The shared rules hold: keys are letters, digits and underscores, not
  starting with a digit, at most 255 characters, matched ignoring case in both
  lookup and the uniqueness check; a value is at most 262,144 bytes; a scope
  holds at most 100 entries; lists are sorted by key ignoring case.
- A secret's value is never returned by any route, log line or audit event. A
  value is encrypted with a deployment key before it reaches any store, so a
  dump of the store yields no secret (decision D10).
- `GET .../vars/resolved` merges the three tiers as specified, with the
  `origin` field and the rule that the user tier is the workspace **owner's**.
- The reserved workspace secret names (`GIT_PAT`, `GIT_USERNAME`,
  `GIT_PASSWORD`, `UPSTREAM_GIT_PAT`, `UPSTREAM_GIT_USERNAME`,
  `UPSTREAM_GIT_PASSWORD`, `FORGE_WEBHOOK_SECRET`) are read for git and forge
  operations with the documented fallback order, and only in workspace scope.

### I. Agent sessions and cost

Covers `api.md` § Agent Session Endpoints.

- Open, complete and report usage for a session, with the documented access
  rules (write access to open; the session's creator or an admin token to
  complete and report), idempotent open by `id`, the terminal statuses, and
  the refusal of `terminated` as a client-set status.
- List and read sessions with `token_summary`, list a session's usage with
  totals over every record whatever the page, and aggregate a workspace's cost
  by `day`, `session` or `model` over a period.
- Session and cost reads need read access **and** an active workspace;
  everyone else, a member of an archived workspace included, gets `403
  workspace access denied`.

### J. Audit, events and the stream

Covers `api.md` § Hub Audit Events, § Audit Ingestion Endpoints, § Audit Query
Endpoints.

- Every operation in the two § Hub Audit Events tables records its event, with
  the specified `event_type`, actor, workspace, resource type and id, action
  and metadata keys — and no others. Recording an event never changes the
  answer of the request that caused it.
- The per-run ingestion routes (events, events batch, session outcomes, tool
  calls, tool errors, traces, traces batch, postmortem) apply the common
  rules: the `run_id` format, idempotency by `id` answering `200` instead of
  `201`, RFC 3339 client timestamps, the batch cap of 1000 with the per-item
  `{accepted, duplicates, errors}` result, and the owner-only access with
  `409 workspace_archived` checked before ownership.
- `GET /api/v1/audit` returns hub and agent events interleaved by instant,
  newest first, with the documented filters and visibility rules.
- `GET .../transcript` rebuilds a node's conversation from its trace events
  with the documented role mapping.
- `GET /api/v1/events` streams `text/event-stream` with a heartbeat at once
  and every 30 s, closes a connection with no reads for 60 s, buffers 256
  events per client and drops the oldest when full, and answers `503` at the
  configured connection limit. The visible workspace set is fixed when the
  stream opens. A client subscribed to a workspace receives an event emitted
  by any component of the deployment, not only the one serving the stream.

### K. Forge integration

Covers `api.md` § Forge Endpoints in full, including § The webhook ingress.

- The hub reaches the forge with the credential it holds for the workspace,
  never the caller's. The § How the hub reaches the forge rules decide the
  repository, the forge kind, the API URL and the credential kind for both
  `origin` and `upstream`; nothing is cached; every request carries
  `User-Agent: af-hub/<version>`, refuses a redirect to another host, port or
  scheme, and has 30 seconds including retries.
- The read routes, the write routes and the webhook routes apply their
  documented order of checks, body limits (65,536 code points on GitHub,
  1,000,000 on GitLab), retry rules and audit rules. Every failure maps to one
  row of the § Failures of the forge table, with the forge's message scrubbed
  of the token and URL userinfo and cut to 1024 bytes.
- The webhook ingress applies its five checks in order, including constant-time
  comparison of GitLab's token and of GitHub's HMAC-SHA256 signature over the
  raw body, and the 1 MiB body limit. A delivery is recorded once per workspace
  and delivery id; a duplicate is `202 {"outcome":"duplicate"}` with nothing
  emitted; of several copies arriving together only one is processed. The
  forge is answered `202` **before** any event is relayed, and the relay then
  happens even if the forge has gone away.
- The GitHub and GitLab event mappings produce exactly the documented hub
  events, in the documented order, with the documented caps (at most 100 label
  events per delivery, at most 100 labels per event). Everything else is
  `ignored`. The payload is never stored or logged.
- Recorded deliveries older than 7 days, and those of workspaces that no
  longer exist, are deleted every hour.

### L. Sandbox routes without a sandbox provider

- With no sandbox provider configured, every route under
  `/api/v1/sandbox-profiles`, `/api/v1/workspaces/:slug/sandboxes...` and
  `/api/v1/sandboxes` answers `404` with `error_type: sandboxes_disabled` and
  the message `sandboxes are not enabled on this hub`, to every authenticated
  caller, before any scope check and before the workspace is looked up. An
  unauthenticated caller still gets `401`.
- `afc sandbox profiles|create|list|get|destroy` therefore exit 1 with that
  message, which is the behaviour `cli.md` § Sandbox Commands specifies for a
  hub without a `[sandbox]` section.

### M. The platform abstraction

- Every Cloudflare product the service uses is reached through one named
  interface defined in the codebase, and no route handler, job or domain
  module imports a Cloudflare type directly. The interfaces cover at least:
  the relational store, the key/value store, the blob store, the queue, the
  per-workspace lock and actor, the scheduled job runner, the event bus behind
  the SSE stream, the secret store, the **repository store** (Artifacts) and
  the **history engine** (the component that computes rebases, merges and
  conflict resolutions).
- A second implementation of every interface exists, runs outside Cloudflare,
  and the whole test suite passes against it. A test that passes against one
  implementation and not the other is a defect in the abstraction.
- That second implementation is **deployable**: a single artefact a person can
  run on one Linux or macOS host, with a relational database, a blob store and
  a git repository store of its own, no Cloudflare account and no network
  access to Cloudflare. The API, CLI and git conformance suites of area O run
  against it and pass, so compatibility is proved on both targets and not only
  on the one the service was written for (decision D6).
- Adding a third deployment target means writing new adapters and a new entry
  point only: no change to a route handler, a job or a domain module.

### N. The web UI

- A landing page describes the service and offers sign-in.
- Sign-in uses the same OAuth flow as the API and ends with the browser
  holding a session. Signing in through the UI does **not** revoke an API key
  the user's automation holds, so a developer may use the dashboard and an
  agent may keep running at the same time (decision D7).
- A dashboard lists the signed-in user's workspaces with slug, status, access
  and last sync, and links to one workspace's view.
- A workspace view shows its fields, and, for a `carry_patch` workspace, its
  patch list with each patch's status and latest rebuild result, the latest
  rebuild, and the most recent audit events.
- The UI reads; it performs no write action. Every change is made through the
  API or `afc`.
- **Look and feel.** The interface takes its visual language from GitHub and
  GitHub Copilot: the same density and restraint, a dark theme and a light
  theme that follow the viewer's system setting, GitHub's type scale and
  spacing rhythm, its status and state colours, and its patterns for a
  repository list, a status badge and a timeline. It is Parley's own product
  and carries Parley's name and marks — it never presents itself as GitHub.
- The UI shows only what the signed-in user's own credential may read: it
  reaches the same API as any other client and is subject to the same access
  rules, so a workspace the user cannot read does not appear.

### O. Conformance

- A conformance suite exercises the documented routes against a running
  deployment and asserts the status, `error_type` and response shape each one
  specifies, including the refusal paths: wrong credential kind, missing
  scope, member write, stranger read, bound token, archived workspace, busy
  workspace.
- A CLI conformance suite runs the real `afc` binary against a running
  deployment and asserts each command's exit code and stdout document,
  covering at least every command in the `cli.md` Quick Tour plus one command
  per command group.
- A git conformance suite drives the real `git` binary against the deployment:
  clone, incremental fetch, push, a push partly refused by push control, a
  push to `refs/hub/replaced/`, and a fetch of a replaced tip.
- A **differential suite** proves the history engine against the real `git`
  binary. For each case in a corpus of at least 200 generated histories —
  covering linear and branching history, renames, binary files, mode changes,
  empty commits, merge commits in the source, conflicting and clean patches,
  and a stack of at least 20 patches — it runs the same rebase or merge both
  ways and asserts that the resulting tree, the parent list, the commit order
  and the set of conflicting files are identical. A case where they differ is
  a failure, not a tolerance.
- **Where each suite runs** (decision D12). `make check`, the fast gate, runs
  the unit tests, the lints and the differential suite against a local `git`.
  The API, CLI and git conformance suites run in `make check-release` and in
  CI, against a deployed Cloudflare preview **and** against the deployable
  second target of area M. `afc` is built from a pinned commit of
  `github.com/agent-fox-dev/hub`. Every suite whose binary, network or
  deployment is missing skips with a message naming what it needed, so no
  absent dependency turns into a failure of the fast gate.

### End-to-end flows

1. **Register and clone.** An operator runs `afc workspace create --slug demo
   --git-url <url> --git-pat <t> --wait`. Parley validates the credential
   against the repository, stores it as the workspace secret `GIT_PAT`, writes
   the workspace with `status: pending`, queues the copy, and answers `201`.
   The repository is copied into the workspace's Artifacts repository,
   `status` becomes `ready` and `head_sha` is set; `afc` polls
   `GET /api/v1/workspaces/demo` and exits 0. The operator then configures the
   credential helper and runs `git clone <hub_url>`: Parley accepts the API
   key over HTTP Basic, admits the caller as the owner, mints a read token for
   the Artifacts repository and relays the fetch, and `git` receives a full
   pack.
2. **Push and merge.** The operator pushes `feature/greeting` to Parley's git
   URL; push control accepts the ref, the update reaches Artifacts, `head_sha`
   is refreshed and `hub.git.push` is recorded. `afc merge submit demo
   --source feature/greeting --target main --wait` queues a merge job; the job
   takes the workspace lock, rebases the source onto the target, fast-forwards
   the target, writes the result back to Artifacts, records `base_sha` and
   `merged_sha` and completes; `afc` polls it to `completed` and exits 0.
   `afc workspace push demo main` pushes `main` to the forge with the stored
   credential and answers `result: pushed`.
3. **Carry-patch rebuild.** `afc workspace sync fork --wait` fetches upstream
   with the upstream credential, writes the new upstream refs into the
   workspace's store, finds upstream advanced, marks one patch
   `merged_upstream`, queues a rebuild and answers with `rebuild_job_id`. The
   rebuild takes the lock at its start, reads the upstream head, applies each
   active patch in order, replays a recorded resolution for one conflict,
   takes the lock again to set the integration branch, marks the merged patch
   `deleted`, closes the positions, and completes. `afc` prints the final
   record and exits 0; `afc workspace patch-status fork` then shows the new
   integration head and each patch's result.
4. **A forge delivery.** GitHub posts a signed `issues`/`labeled` delivery to
   `POST /hooks/forge/widgets`. Parley reads the workspace's
   `FORGE_WEBHOOK_SECRET`, verifies the HMAC in constant time, records the
   delivery id once, answers `202 {"outcome":"relayed"}`, and then relays
   `hub.forge.issue.labeled`, which reaches a client holding
   `GET /api/v1/events?workspace=widgets` and appears in
   `GET /api/v1/audit` and in `afc forge deliveries widgets`.
5. **A bound token is refused.** An agent holding a PAT bound to `widgets`
   calls `GET /api/v1/workspaces/other`. Parley refuses with `404 workspace
   not found` before any scope, existence or status check, and records one
   `hub.token.bound_refused` event with `requested_workspace: other`,
   `status: 404` and `surface: api` — and suppresses further refusals by the
   same token for a minute, counting them in `suppressed`.

## Design Decisions

1. **D1 — This PRD covers the whole port.** All fifteen requirement areas, in
   one document. At roughly fifteen areas it is far past what one spec package
   holds (10 requirements, 12 tasks), so `spec` will split it into something
   like a dozen packages, the first foundational. Chosen by the project owner
   over a foundation-only PRD, so that the whole contract is specified against
   one consistent reading of `api.md`.
2. **D2 — Cloudflare Artifacts is the repository store.** Chosen by the
   project owner. It gives a durable repository per workspace, the full git
   smart HTTP protocol, repo-scoped tokens and a Worker binding, which is the
   whole transport and storage half of the hub's git server. It gives no
   history operations, no pre-receive hook and no credentialed import, which
   is what D14, D15 and the push-control requirement in area D answer.
3. **D3 — Both git wire protocol versions are served.** `api.md` says the hub
   speaks v0 and that a v2 client falls back; Artifacts speaks v1 and v2.
   Parley serves whichever the client asks for. The `api.md` sentence
   describes the old implementation, not a promise a client depends on, and
   refusing v2 would mean degrading a protocol the store already speaks.
   Recorded as a divergence below.
4. **D4 — Sandboxes are served as disabled.** Chosen by the project owner. No
   provider and no gateway are implemented; every sandbox route answers
   `404 sandboxes_disabled`. This is not a gap in the port: `api.md` defines
   it as the behaviour of a hub with no `[sandbox]` section, and `cli.md`
   defines what `afc sandbox` does against such a hub, so the port stays
   compatible. The hub's gateway is gRPC, which Workers do not serve
   natively, so a Cloudflare sandbox provider is its own PRD.
5. **D5 — Identity is reimplemented, not wrapped.** The hub's identity layer
   is the Go library `apikit`. There is no TypeScript equivalent, so Parley
   implements those routes from `api.md` § Identity and Access Endpoints. The
   credential string formats and the key-revoking login behaviour are kept
   because `afc` and existing automation depend on them.
6. **D6 — Portability is proved by a deployable second target.** Chosen by the
   project owner over a local-only second implementation. Every platform
   interface gets a second implementation, and that implementation is a
   single artefact runnable on one host with no Cloudflare account, against
   which the API, CLI and git conformance suites also pass. A seam that is
   only asserted is usually wrong; running the same conformance suites on two
   real targets is what makes G4 a measured claim instead of an intention. It
   costs a second deployment story, accepted deliberately. No Kubernetes
   manifests are written in this work — the second target is the proof that
   they would be adapters and a entry point, nothing more.
7. **D7 — The UI reads only, in GitHub's visual language.** Chosen by the
   project owner. A read-only dashboard keeps the UI out of the compatibility
   surface — every write stays on the API that `afc` and the conformance
   suite already cover. The browser session is distinct from the API key
   because `POST /api/v1/auth/callback` revokes every existing API key of the
   user, so a shared credential would mean a web sign-in silently killing a
   running agent. The interface follows GitHub's and GitHub Copilot's visual
   language, because that is what this audience already reads fluently; it
   carries Parley's own name and marks and never presents itself as GitHub.
8. **D8 — `GET /version` reports `runtime_version`, not `go_version`.** Chosen
   by the project owner. Reporting a JavaScript runtime under a key named
   `go_version` would be a lie a future reader has to decode, and no
   documented client reads the field — `cli.md` gives `afc` no command that
   does. Recorded as a divergence below.
9. **D9 — `GET /metrics` is served in full.** The documented names, types and
   labels are kept, and a metric the port has no source for reports 0 rather
   than vanishing, which is what `api.md` already specifies for
   `afhub_jobqueue_depth`. Cloudflare's own analytics do not replace a route
   that a deployment's scraper is configured to read.
10. **D10 — Secrets are encrypted before they reach any store.** With a
    deployment key, so that a dump of the database or the KV namespace yields
    no secret value. `api.md` requires only that values are never returned;
    this is stricter, because on a hosted platform the store is more exposed
    than a hub's own disk was.
11. **D11 — The Makefile and its target names are kept.** Chosen by the
    project owner: `build`, `test`, `fmt`, `vet`, `lint`, `check`,
    `check-release`, `tidy`, `clean`, implemented with the TypeScript
    toolchain, so one vocabulary holds across the agent-fox repositories and
    `make check` stays the only gate an agent runs. `make doc` and
    `docs/generated/` are dropped: they exist to hold `go doc` output and have
    no counterpart here. This is a divergence from `.specs/steering.md`
    § Generated Docs, recorded below.
12. **D12 — The conformance suites run in the full gate and skip when they
    cannot run.** Chosen by the project owner. See area O for where each suite
    runs. `afc` is built from a pinned hub commit; a missing binary, network
    or deployment is a skip with a message, never a failure, so the fast gate
    stays runnable on a laptop and offline.
13. **D13 — Parley serves its own git URL and relays to Artifacts.** The
    contract fixes `hub_url` under the hub's own host, HTTP Basic with a hub
    credential, the hub's three-level access rule, PAT git scopes, push
    control and the protected `refs/hub/replaced/` namespace. Artifacts
    authenticates with its own repo-scoped tokens and enforces none of that,
    so clients may never be pointed at an Artifacts remote directly, and an
    Artifacts token never leaves the deployment.
14. **D14 — History is computed in TypeScript; nothing runs `git`.** Chosen by
    the project owner over driving a real `git` binary in a Cloudflare
    Container. The deployment stays serverless, with no container images, no
    cold-start tier and no second runtime to operate. The cost is accepted
    deliberately: `isomorphic-git` offers neither rebase nor rerere, so both,
    and the two merge strategies, are new TypeScript code, and new code that
    must match `git` exactly is only as good as its evidence — hence the
    differential suite in area O, which is a requirement of the port and not
    a nice-to-have. The second cost is that Worker CPU and memory bound the
    work, hence the stated sizes in D17.
15. **D15 — Every repository is copied in by Parley; `import()` is not used.**
    Artifacts' `import()` takes a public HTTPS URL, carries no credential and
    runs once, so it cannot serve the ordinary case of a private repository
    and cannot serve any later sync. Rather than keep two paths and exercise
    the private one less, Parley creates an empty Artifacts repository and
    performs the initial copy itself, with the workspace's credential, by the
    same machinery every sync uses — one path, always exercised, identical
    behaviour for a public and a private remote. If the measured sizes of D17
    show this path cannot handle large public repositories, `import()` is the
    documented fallback for exactly that case.
16. **D16 — The `refs/hub/replaced/` namespace is verified before it is
    relied on.** Artifacts documents branches, tags and `refs/notes/*` and
    says nothing about other namespaces, and documents neither force push nor
    ref deletion — all three of which the contract needs. The first spec
    package that touches the store establishes what the store actually
    accepts, as its first task, and records the answer in an ADR. If
    arbitrary namespaces are refused, replaced tips are kept as git notes or
    in the deployment's own store, `GET .../patches/:id` still reports
    `replaced_sha` and a client push to `refs/hub/replaced/` is still
    refused with the documented message — and `git fetch <hub_url>
    refs/hub/replaced/<branch>` becomes the one documented operation the port
    cannot offer. **Provisional**, because it rests on documentation that
    does not address the question.
17. **D17 — The sizes at which history operations work.** Proposed, and
    **provisional**: 500 MB of packed objects and 10,000 refs per repository
    for copy, sync and push; a stack of 50 patches with at most 1,000 changed
    files and 10 MB of diff per patch for a rebuild; 20 branches for a batch
    rebase; `dead_letter` after 15 minutes without progress; `failed` after
    30 minutes in `cloning`. The requirements have to assert numbers, so these
    are numbers — but none has been measured, because no code exists yet. The
    first spec package that touches the history engine should measure the real
    ceilings, and these figures should be corrected then rather than defended.

**Recorded divergences from `api.md`.** Every other behaviour of the reference
is a requirement; these five are the whole list, and nothing else may be added
to it without a decision here.

| Divergence | Why | Decision |
|---|---|---|
| `GET /version` reports `runtime_version` in place of `go_version` | The runtime is not Go and no documented client reads the field | D8 |
| The git server serves protocol v2 as well as v0 | The store already speaks v2; refusing it would degrade the protocol | D3 |
| The sync message `failed to reset working tree for patch branch <branch>` can never occur | There is no worktree: the store holds history, not checked-out files. The other two refresh-failure messages are unaffected | D14 |
| `git fetch <hub_url> refs/hub/replaced/<branch>` may be unavailable | Only if the store refuses the namespace; the `replaced_sha` field and the push refusal are kept either way | D16 (provisional) |
| `make doc` and `docs/generated/` do not exist | They hold `go doc` output and have no counterpart in TypeScript. A divergence from `.specs/steering.md`, not from `api.md` | D11 |

Serving every sandbox route as `404 sandboxes_disabled` is **not** a
divergence: `api.md` defines it as the behaviour of a hub with no `[sandbox]`
section, and `cli.md` defines what `afc sandbox` does against such a hub.

## Dependencies

- **`docs/prd/api.md`** and **`docs/prd/cli.md`** in this repository are the
  normative contract. They are the input `spec` must read; no requirement may
  contradict them.
- **`.specs/steering.md`** (library reuse, target platforms, the `make check`
  rule, documentation freshness and style, test organisation). Its Go-specific
  rules (`go.mod`, `make doc`, `docs/generated/`) do not apply; see D11.
- **Cloudflare Artifacts** — **open beta**, Workers Paid plan only, billed
  from 14 October 2026, 1 GB per repository created on or after 29 September
  2026. A beta service is the port's foundation: a change to its binding, its
  limits or its availability is a risk to this work, and the platform
  abstraction (area M) is what keeps that risk contained.
- **`isomorphic-git`** — the git implementation the history engine is built
  on (decision D14), for object access, fetch and push. It provides no rebase
  and no rerere, so those are Parley's; its documentation says it cannot use
  IndexedDB or `lightning-fs` in Workers, so the engine needs its own storage
  adapter. Its behaviour under the port's stated sizes is unmeasured and is
  the single largest technical risk in this work.
- **The real `git` binary** — not a runtime dependency, but a **test**
  dependency: the differential suite (area O, goal G6) cannot run without it.
- **`github.com/agent-fox-dev/hub`** — the Go service being ported and the
  source of the `afc` binary used for conformance. Not a spec package; an
  external repository. It was **not reachable** from this session (TLS
  failure on `api.github.com`), so nothing in this document was read from it.
- **No spec packages exist** (`speca --list --all` reports 0), so nothing here
  depends on or supersedes one.
- **External services at deployment time:** Cloudflare (Workers, Artifacts,
  and whichever of Durable Objects, D1, R2, Queues, Cron Triggers and
  Containers the architecture chooses) and an OAuth provider (GitHub by
  default) for login.

## Verified External API

Nothing could be verified against installed code: the repository has no
package manifest and no dependencies. What follows was read from Cloudflare's
own documentation during this session. Every signature a spec package carries
from here must be re-verified against the installed binding types before it is
relied on.

**Cloudflare Artifacts, Workers binding** — read from
`developers.cloudflare.com/artifacts/llms-full.txt`. Namespace binding
(`env.ARTIFACTS`):

| Member | Signature as documented |
|---|---|
| `create` | `create(name, opts?: {readOnly?, description?, setDefaultBranch?}) => Promise<ArtifactsCreateRepoResult>` — returns `name`, `remote`, `defaultBranch`, `token` |
| `get` | `get(name) => Promise<ArtifactsRepo>` — a disposable handle; throws when the repo is missing or not ready |
| `list` | `list(opts?: {limit?, cursor?}) => Promise<ArtifactsRepoListResult>` — each repo has `status` of `ready`, `importing` or `forking` |
| `import` | `import(params: {source: {url, branch?, depth?}, target: {name, opts?: {description?, readOnly?}}}) => Promise<ArtifactsCreateRepoResult>` — **public HTTPS remotes only, no credential field**; `REMOTE_AUTH_REQUIRED` when the remote needs one; `409` while a repo is importing or forking |
| `delete` | `delete(name) => Promise<boolean>` |

Repository handle:

| Member | Signature as documented |
|---|---|
| `info` | `info() => Promise<ArtifactsRepoInfo>` — throws `NOT_FOUND` when deleted |
| `createToken` | `createToken(scope?: "read" \| "write" = "write", ttl?: seconds) => Promise<ArtifactsCreateTokenResult>` — `plaintext`, `expiresAt`; TTL 60 s to 31,536,000 s, default 86,400 s |
| `listTokens` / `revokeToken` | `listTokens() => Promise<ArtifactsTokenListResult>`; `revokeToken(tokenOrId) => Promise<boolean>` |
| `fork` | `fork(name, opts?: {description?, readOnly?, defaultBranchOnly?}) => Promise<ArtifactsCreateRepoResult>` |
| `log` | `log(opts?: {ref = "HEAD", limit = 50 (max 1000), offset?}) => Promise<ArtifactsCommitMetadata[]>` — first-parent chain; `[]` when the ref does not resolve |
| `readCommit` / `readTree` / `readBlob` | `readCommit(hash) => Promise<ArtifactsCommitMetadata \| null>`; `readTree(hash) => Promise<ArtifactsTreeEntry[] \| null>` (immediate children only); `readBlob(hash) => Promise<Blob \| null>` (no MIME type) |
| `readFile` | `readFile({ref, path}) => Promise<Blob \| null>` — throws `INVALID_INPUT` on an empty `ref` or `path`; `null` for a missing path or a directory |

**There is no object-level write API**; everything is written by pushing over
the git protocol. **There is no server-side merge, rebase or rerere, and no
pre-receive hook**; the only push signal is the `cf.artifacts.repo.pushed`
event. Events, deliverable to a queue: `cf.artifacts.repo.created`,
`.deleted`, `.forked`, `.imported` (account level) and `.pushed`, `.cloned`,
`.fetched`, `.token.created`, `.token.revoked` (repo level). Git tokens are
repo-scoped with `read` (clone, fetch, pull) or `write` (also push). The REST
API covers namespace creation (with an `eu` or `us` `jurisdiction`), repo
create, get, import and token minting with a Cloudflare API token.

**NOT FOUND / unverified:**

| Thing | Status |
|---|---|
| Support for ref namespaces other than branches, tags and `refs/notes/*` — the contract needs `refs/hub/replaced/` | **NOT FOUND** in the documentation. Assumed unsupported until verified; see D16 |
| Force push and ref deletion over the git protocol | **NOT FOUND**. Both are needed (mirroring patch branches, forcing the integration branch, deleting branches) |
| Repository, ref-count and per-namespace request-rate limits | **NOT FOUND**, beyond the 1 GB storage limit and the token TTL range |
| Artifacts pricing rates | **NOT FOUND**; billing begins 14 October 2026 on Workers Paid |
| `isomorphic-git` in Workers: no IndexedDB, `lightning-fs` unusable, a custom `fs` adapter needed, no rebase, no rerere | Read from `isomorphic-git.org/docs/en/next/guide-cloudflare-workers`; **unverified** |
| Worker CPU and memory ceilings, and whether a rebuild of the sizes in D17 fits inside them | **NOT MEASURED.** The limits that decide whether D14 and D17 hold at all |
| Hono, React, Vite, Durable Objects SQLite, D1, R2, Queues, Cron Triggers, Workflows | **NOT READ.** Named in `docs/prd/start-here.md` as suggestions |

Nothing in `github.com/agent-fox-dev/hub`, `github.com/agent-fox-dev/agentkit-go`
or `github.com/cloudflare/workerd` could be read: GitHub was unreachable from
this session.
