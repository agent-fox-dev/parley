---
spec_id: "55"
spec_name: "acceptance_suite"
title: "Prove the product with black-box acceptance tests"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "d262cbe74cd82375ff5773accc145407b961b846ad4e85e82a6a36895535ceda"
schema_version: 3
source: "docs/prd/16_acceptance.md"
---

# Prove the product with black-box acceptance tests

## Intent

Parley is accepted when a fixed list of product-level outcomes holds on a running hub, shown from the outside. This package is the suite that shows it: black-box scenarios that start a real hub and prove each acceptance criterion of the product using only the REST API, the `parley` CLI and stock `git`, so that "the product works" is a test result and not a claim.

## Goals

- A suite under `test/acceptance/` that builds the `parleyd` and `parley` binaries, starts a hub from one configuration file and one data directory, and drives it only over HTTP, through the `parley` binary and through stock `git`.
- One named scenario test for every statement of acceptance criteria 1 to 14 and 16 to 18 of §16, so that a failing criterion is a failing test with that criterion's name.
- One representative end-to-end journey for each carried feature of criterion 15 (workspaces, carry-patch workspaces, secrets and variables, forge integration, sessions with audit and metrics).
- `go test ./test/acceptance/... -count=1` passes on a developer machine with `git` 2.38 or later and no other service; scenarios that need something more (an OpenShell gateway, a repository of 1 GiB, the reference host, a wait longer than two minutes) are skipped with a reason that names the environment variable that enables them.
- A scenario never reads the hub's stores or repositories on disk and never imports a hub package that holds product behaviour.

## Non-goals

- **Every listed success and error case of the carried features' routes and commands** (criterion 15), and the last sentence of criterion 18 for the sandbox routes and commands. They are satisfied by the feature packages' own checks: packages 10 to 16 (workspaces and forks), 31 to 38 (carry-patch), 39 and 40 (secrets and variables), 41 to 43 (forge), 44 to 48 (sandboxes), 49 to 51 (sessions, telemetry, audit and metrics). This suite covers one journey per carried feature and does not restate their cases.
- **The removal of a fork 24 hours after all its changes have landed or been abandoned** (part of criterion 3). The grace period is fixed at 24 hours and a black-box test cannot move the hub's clock. Not covered by this package; package 16 task_fork_lifetime checks it with a controlled clock. This suite proves the removal at the TTL.
- **The capacity targets of §15.4** (5,000 active forks, 2,000 open changes per workspace, 500 event streams, 8 land attempts computing, mergeability of 100 open changes within 60 s, and the three sandbox figures). Not covered by this package; the performance harness of package 54 operations_and_packaging measures them. This suite measures the six latency and throughput targets a single client can observe.
- **The default sandbox limits as numbers** (the fifty-first sandbox of a workspace family, 200 per hub, 100 per organization, 20 per user). The suite proves each of the four limits with the limit lowered in configuration; the defaults are checked by package 44 sandbox_runtime_and_profiles.
- **The behaviour itself.** Every route, command, push rule and background task the scenarios exercise is specified and unit-tested by packages 01 to 54. This package adds no route, no command, no configuration key and no product code.
- **The container image, the reference sandbox image and the Kubernetes manifests** (NFR-006): package 54.
- **A test double of the OpenShell gateway.** Sandbox scenarios run against a real gateway or do not run.

## Background

The repository holds no code yet; every file named here is new. §16 of the product PRD lists 18 acceptance criteria and says they are "demonstrated by black-box tests against a running hub using only the REST API, the `parley` CLI and stock `git`". The feature packages 01 to 54 each carry their own unit and integration checks beside their code, and end-to-end tests under `test/e2e/` that start the hub in-process. None of them proves a criterion of §16 as a whole: a criterion such as "twenty changes are landed concurrently, all land, the history is linear" crosses the git server, changes, landing and events.

The suite lives in its own directory, `test/acceptance/`, apart from `test/e2e/`, because it obeys a stricter rule: it runs the built binaries as an operator would and may touch nothing but the three public surfaces.

What the suite builds on, all delivered by other packages:

- `cmd/parleyd` and `cmd/parley` (package 01), the configuration file and the first-boot admin token (package 03), `parleyd backup <directory>` (packages 03 and 54).
- The listening line `parleyd` writes to stderr once it accepts connections (package 01), and `GET /readyz`.
- `[[oauth.providers]]` with `token_url` and `userinfo_url` overrides (package 04): the only way to obtain an API key is the OAuth callback (PLAT-FR-033), so the suite runs a fake identity provider.
- The test origin of `internal/gitrepo/gitrepotest/` (packages 10 and 13): a bare repository served over TLS, read-only or accepting pushes. It stands in for the origin, which is outside the hub.

## Requirements

### The harness

A scenario gets its hub from one helper. The helper:

1. builds `./cmd/parleyd` and `./cmd/parley` once per test run into a temporary directory;
2. creates a temporary configuration directory and a temporary data directory, writes `config.toml` with `[server] bind = "127.0.0.1"`, a free port, `external_url` naming that address, the `[[oauth.providers]]` entry of the fake identity provider, and whatever keys the scenario adds;
3. runs `parleyd --admin-email` once to obtain the admin token file, then starts `parleyd` with `XDG_CONFIG_HOME` and `XDG_DATA_HOME` set to the two directories and `PARLEY_SECRETS_KEY` set, waits for the listening line and for `GET /readyz` to answer 200;
4. stops the hub with SIGTERM when the scenario ends, and can stop it with SIGKILL and start it again on the same directories at any point.

Beside the hub the harness offers: a fake identity provider that turns a name into a signed-in user with an API key through `POST /auth/callback`; a test origin with a seeded history; a function that runs the `parley` binary with a credential and returns exit status, stdout and stderr; a function that runs stock `git` with an empty home, no system configuration, no terminal prompt, and either a credential in the remote URL or `parley credential-helper` configured; a JSON HTTP client; an event-stream reader that records every frame with its `seq`; a webhook receiver whose answers the scenario scripts; and a fake forge API.

Three kinds of scenario do not run everywhere. A sandbox scenario runs only when `PARLEY_ACCEPTANCE_OPENSHELL_GATEWAY_URL` is set (with `PARLEY_ACCEPTANCE_OPENSHELL_TOKEN_FILE` and `PARLEY_ACCEPTANCE_SANDBOX_IMAGE`). A scenario that waits more than two minutes runs only with `PARLEY_ACCEPTANCE_LONG=1`; one that builds a repository of 1 GiB only with `PARLEY_ACCEPTANCE_LARGE_REPO=1`; one that measures performance only with `PARLEY_ACCEPTANCE_REFERENCE_HOST=1`. Without its variable a scenario is reported as skipped, with a reason that names the variable; it never passes silently and never fails.

### Changes and stacks (§16 criteria 1 and 2)

- A branch pushed with `git push -o parley.change` becomes a change with a number. Four more pushes, one of them a force push of rewritten history, give versions 2 to 5 of the same number; `git fetch` of `refs/parley/changes/<number>/<1..5>` brings each of the five heads.
- A version that is a pure rebase of its predecessor has `patch_equivalent_to` set and an empty interdiff (`GET …/changes/:number/diff?against=`).
- A commit landed on the protected default branch resolves, through `GET …/changes/by-commit/:sha` and `GET …/scm/commits/:sha`, to its change, version and session. A commit that reached the default branch by the owner's direct push resolves to `change: null`, and the event log holds its `parley.git.push` with `direct: true`.
- Three branches, each built on the one before, pushed with `parley.change`, open three changes linked by `depends_on`. A new version of the bottom one produces, without any client action, a version of kind `restack` on each of the two above. Landing the bottom one rebases the next onto the target and clears its `depends_on`.
- When the restack cannot apply, the dependant has `needs_restack: true`, `restack_reason: "conflict"` and a conflict report, and neither its source branch nor its version number has moved.

### Task forks (§16 criterion 3)

- `POST /workspaces/:slug/forks` with a `token` object answers `201` with the fork, its `remote` and a token value. A `git clone` of `remote` with that token succeeds immediately, and so does a push of a new branch.
- With a parent repository of at least 1 GiB the same holds, and fork creation takes under one second for the 1 GiB parent and for a 1 MiB parent alike.
- The fork's token is answered `404 not_found` on another workspace and `403 token_bound` on a route of the parent it may not use (for example `GET …/scm/refs` of the parent). It reaches its own change on the parent, and the parent's `GET …/activity` and `GET …/policy`.
- A push to a tracking branch of the fork is refused with `tracks the parent workspace and is read-only in this fork`.
- A fork created without `ttl` has `expires_at` exactly 7 days after `created_at`.
- A fork created with `ttl` `5m` answers `404` within 7 minutes of its creation, its token no longer authenticates on it, and `parley.fork.expire` with `reason: "ttl"` is in the event log.

### Source control API and git compatibility (§16 criteria 4 and 14)

- Without a clone: `GET …/scm/file`, `…/scm/tree`, `…/scm/commits?path=`, `…/scm/diff` and `…/scm/blame` return the content the seeded origin holds.
- `POST …/scm/commits` with operations on several files creates one commit on a new branch; a second call with the previous `expected_head_sha` answers `409 ref_moved` with the current head in `details`, and the branch has not moved.
- A change is opened and landed through the API alone, and the default branch then holds the files.
- A repository in which the same file operations were made with `git commit` has the same tree id at its head as the hub's branch.
- Stock `git` clones in full, with `--depth 1` and with `--filter=blob:none`, fetches and pushes, authenticating with a PAT in the remote URL and with `parley credential-helper`.
- For a caller without read access, the answers for an existing workspace and for a slug that does not exist are the same in status, body and `WWW-Authenticate`, over the API and over git.

### Landing and conflicts (§16 criteria 5 and 6)

- Twenty changes that each add a different file are landed by twenty concurrent `POST …/land` calls. Each call is answered `200` with status `landed` on its first request; the target's first-parent history has no merge commit and holds each change's file exactly once.
- Two changes that edit the same lines are landed concurrently. Exactly one is `landed`. The other is answered `409 land_conflict` with a conflict report whose `files` name the file and whose `landed_changes` name the winner; the target holds the winner's content only.
- A hub stopped with SIGKILL while lands are in flight is started again. For every change: either it is `landed`, its commit is on the target and one `parley.change.land` event names it, or it is not landed, the target does not hold its file and no such event exists. No attempt is left `running`.
- For the conflicted change, `GET …/conflict/file?path=` returns the file with conflict markers; `parley change resolve` with the resolved content produces a new version; the change then lands.

### Policy, approvals and path-scoped tokens (§16 criteria 7 and 8)

With a policy of rules `docs` (`docs/**`, nothing required), `code` (`src/**`, check `test`), `billing` (`billing/**`, one approval from `owner`) and `secrets` (`deploy/secrets/**`, `block: true`):

- a change touching only `docs/` is `ready` and lands with no approval;
- a change touching `src/` is `pending` until `PUT …/checks/test` reports `passed`, then `ready`;
- on a change touching `billing/`, an approval recorded with the token that pushed it leaves the requirement unsatisfied (`counts_for` empty); one recorded with the owner's API key satisfies it;
- a change touching `deploy/secrets/` is `blocked`; a land with `override` by the owner is answered `409 policy_blocked` and the target does not move;
- the counted approval is still counted on a version that is a pure rebase, and is not counted on a version with other content;
- `POST …/policy/evaluate` with each of the four path sets, sent before any of the changes exists, answers the status the change later has.

A token with `paths: ["a/**"]` pushes a branch that changes `a/x` and commits `a/y` through the API. A push and an API commit that touch `b/x` are refused, the push with `path b/x is outside this token's allowed paths` on the client's stderr and the API with `403 path_not_allowed` whose message names `b/x`; no ref moves.

### Protected branches, automatic landing and revert (§16 criteria 9 and 10)

- The default branch moves when a change lands and when `parley workspace sync` brings a new commit from origin.
- With the owner's API key, a fast-forward `git push`, `POST …/scm/commits` on the branch and a fast-forward `PATCH …/scm/refs/heads/<branch>` each succeed, and each produces `parley.git.push` with `direct: true`. The admin token's fast-forward push succeeds the same way.
- The same three operations are refused (`403 protected_branch` over the API, `is protected; push another branch and open a change` over git) with the owner's workspace-bound token, the owner's path-scoped token and an org member's API key.
- A forced update and a delete are refused with every credential, the admin token included (`is protected and cannot be force-pushed or deleted`).
- After `protect.owner_push` is set to `false`, every direct update is refused for every credential, the owner's API key and the admin token included.
- A change with `auto_land` whose policy requires check `test` lands with no land call once the check is reported `passed`; its land attempt has `trigger` `auto`.
- `POST …/changes/:number/revert` on a landed change opens a change; after that one lands, the target's tree id equals the tree id before the original landed.

### Awareness and sessions (§16 criteria 11 and 13)

- Two open changes that touch the same file: the event log holds exactly one `parley.change.overlap` for each of the two, naming the other.
- `GET …/activity?paths=<file>` returns both changes and an active claim on that path. After the claim's `ttl` of 2 seconds has passed, the same call returns both changes and no claim.
- With the overlap and the claim in place, a push to either source branch and a land of either change succeed.
- A session opened with `parley session start` is named on a fork creation, on pushes, and on a land. `GET /sessions/:id` then lists the fork and the changes; the changes' versions carry the session, and `GET …/lands` shows the land attempt with it.
- `GET …/scm/commits/:sha` of the landed commit names the session.

### Events and webhooks (§16 criterion 12)

- A consumer reads `GET /events` up to a `seq`, disconnects, fifty events are written, and it reconnects with `Last-Event-ID`. It receives exactly those fifty in ascending `seq`, then an event written after the reconnect, on the same connection.
- A resume position that has left the event log answers `410 cursor_expired` with `oldest_seq`. (The shortest `[events].retention` is 1 hour and retention runs at start and hourly, so this scenario waits about 65 minutes and restarts the hub once.)
- `parley.git.push`, `parley.workspace.fork`, `parley.change.version`, `parley.change.land` and `parley.change.conflict` each carry the `session_id` given on the operation that caused them.
- A hub killed with SIGKILL while eight writers create claims is started again: every claim the hub holds has its `parley.claim.create` event and every such event has its claim; `seq` values are strictly increasing across the restart.
- A webhook subscription delivers each workspace event as a `POST` whose `Parley-Signature` verifies with the `secret` returned at creation, in `seq` order.
- While the receiver answers `500`, it sees the same `Parley-Delivery` again, the second attempt not earlier than 10 seconds after the first, and no request for a later `seq`. Once it answers `200`, every held event arrives in `seq` order with none missing.
- Creating a subscription with a URL on a private address (`https://10.0.0.5/hook`) answers `400 webhook_destination_refused`.
- A ping and a redelivery each arrive with a verifying signature; the redelivery carries the `Parley-Delivery` of the original.

### Conventions, operations and carried features (§16 criteria 15, 16 and 17)

- For every route of the route index (Appendix A) under `/api/v1`: a request without a credential (for the public `POST /auth/callback`, a malformed body) and a request with a method the path does not have are answered with the error envelope: `application/json; charset=utf-8`, `error.code` equal to the status, a non-empty `error.error_type`; the wrong method is `405 method_not_allowed`.
- Every list route answers `{items, next_cursor}`; with `limit=1` and three items, following `next_cursor` returns each item once and ends with `next_cursor: null`; `limit=0` and `limit=201` are `400 invalid_request`.
- Every creating `POST` replays under the same `Idempotency-Key`: status `200`, the original body without a show-once member, and nothing created twice.
- The hub the harness starts consists of one binary, one `config.toml` and one data directory; nothing else is written outside them.
- `parleyd backup <directory>` on a running hub under writes produces a snapshot; a second hub started on that snapshot answers `/readyz` with 200 and serves the workspaces, changes and repositories the snapshot holds.
- On the reference host (8 cores, 32 GB, local SSD): fork creation p95 under 1 s; file and tree read p95 under 100 ms; API commit of 100 files p95 under 500 ms; land of a 20-commit change p95 under 2 s; at least 60 lands in one minute on one target; an event visible to a live subscriber within 1 s.
- One journey per carried feature: a workspace is created with `parley workspace create --wait`, synced, archived, reactivated and deleted; a carry-patch workspace gets a patch and a rebuild whose integration branch is upstream plus the patch; a secret is stored and listed without its value while a variable resolves with its value; `parley issue list` returns the issues of a fake forge; a session with a usage report appears in `parley audit query` and in `/metrics`.

### Sandboxes (§16 criterion 18)

All against a hub configured with the OpenShell gateway of the environment.

- One fork-create call with a `sandbox` object returns a fork and a sandbox. When the sandbox is `ready`, an exec of `git status` in its working directory shows the fork's branch, and a `git push -o parley.change` run inside it opens a change in the parent.
- An exec of `env` shows a placeholder, not the value, for `PARLEY_API_KEY` and for an attached credential. With a profile whose `expose_secrets` is empty, and for a fork owned by an org member with read-only access under a profile that exposes a parent secret, no secret value is in the environment.
- With a profile that exposes a secret, an exec reads its real value; `GET /sandbox-profiles`, the sandbox object and `parley.sandbox.create` name the secret and do not contain the value; `GET …/logs` shows `[redacted]` in its place.
- Refusals: creation with a required exposed secret missing is `422 credential_missing`; approving a proposal with a workspace-bound token is `403 token_bound`; an exec with a token that lacks a scope of the sandbox's hub credential is `403 insufficient_scope`.
- A request from the sandbox to a host no rule allows fails and `parley.sandbox.deny` names the host. After the owner approves the proposal for that host, the same request succeeds.
- Approved with `scope: workspace`, the rule appears in `GET …/sandbox-rules`; a sandbox created afterwards on the workspace and one on another fork reach the host with no proposal; a sandbox that was running before does not; after the rule is deleted a new sandbox is blocked again.
- With each of `max_per_workspace`, `max_sandboxes`, `max_per_org` and `max_per_user` set to 1 in turn, a second sandbox is refused with `429 sandbox_capacity` whose message names that limit.
- A requested stop, an idle stop and expiry each leave a ref under `refs/parley/sandboxes/<id>/`; a sandbox created with `restore_from` has the committed and the uncommitted file back. After the hub is stopped and started, a sandbox that was running is `ready` again and a file its main process keeps writing has kept growing.

### End-to-end flows

1. **An agent forks, pushes and lands.** A developer registers the origin with `parley workspace create --wait`. An agent creates a fork with a token in one call, clones the fork's remote with stock git, commits, and pushes with `-o parley.change`. The hub opens a change in the parent. The agent lands it with `parley change land`; the hub rebases, moves the default branch and writes `parley.change.land`. A fresh clone of the parent holds the agent's file.
2. **Two agents race to land.** Two agents open changes that edit the same lines and call land at the same moment. The hub lands one and answers the other `409 land_conflict` with a report. The losing agent fetches the conflicted file with markers, sends a resolution with `parley change resolve`, and lands.
3. **A consumer resumes the event stream.** A review agent follows `GET /events`, disconnects, and reconnects with `Last-Event-ID`. The hub replays what was missed in order and continues with live events.

## Design Decisions

1. **The suite lives in `test/acceptance/`, apart from `test/e2e/`; both exist on purpose.** `test/e2e/` belongs to the feature packages 01 to 54: each adds its own end-to-end tests there, which start the hub in-process through the run function of package 01 and may use a controlled clock and fakes. `test/acceptance/` belongs to this package alone and is the final black-box suite: it runs the built binaries, because criterion 17 ("one binary, one configuration file, one data directory") and the SIGKILL scenarios need a real process. This package adds no file under `test/e2e/` and no feature package adds one under `test/acceptance/`.
2. **Black-box means three surfaces.** A scenario may use HTTP, the `parley` binary and stock `git`, and may start, stop and kill the `parleyd` process and run `parleyd backup`. It reads no file under the data directory except to hand the directory to `parleyd`. The harness imports no package under `internal/` except `internal/gitrepo/gitrepotest`, which serves the origin, a system outside the hub.
3. **API keys come from a fake identity provider.** PLAT-FR-033 allows no other way to create one. The provider is configured through the `token_url` and `userinfo_url` overrides of `[[oauth.providers]]`.
4. **No clock control.** The hub under test is a separate process with the system clock. Timed behaviour is shown in real time where the shortest configurable wait allows it: a claim with `ttl` `2s`, the webhook retry after 10 s, a fork with `ttl` `5m`, an event retention of `1h`. Scenarios that wait more than two minutes run only with `PARLEY_ACCEPTANCE_LONG=1`. The fixed 24-hour grace period of early fork removal is left to package 16 (see Non-goals).
5. **"Identical repository" is compared by tree id.** Commit ids differ by author time and committer, so criterion 4 compares the tree id of the branch head and the list of changed paths per commit, not commit ids.
6. **Gated scenarios are skipped, never failed and never silently passed.** The skip reason names the environment variable. This is what the PRD's "demonstrated against a hub connected to an OpenShell gateway" and "on the reference host" become on a machine that has neither.
7. **Sandbox limits are proved with lowered limits.** Creating 51 or 201 sandboxes on a shared gateway is not a test a suite can run routinely; each limit is set to 1 and the second sandbox must be refused naming it.
8. **The suite measures six of the §15.4 targets** — those one client can observe through the REST API — with 100 samples each (one minute for the throughput target). The capacity targets belong to package 54's harness.
9. **The conventions scenarios are driven by the route table generated from the PRD's route index (`docs/prd/appendix_a_routes.md`)**, because the hub has no route that lists its routes. The table is kept in the suite; no number of rows is fixed here. The scenario reads the route index and asserts that the table holds exactly its routes, so a route added to or removed from the index fails the suite until the table is generated again.
10. **Each scenario starts its own hub** on its own port and directories, so scenarios run in parallel and a killed hub affects one scenario.
11. **The suite runs under the plain `go test ./...`.** It has no build tag: a default run takes minutes, not hours, because everything long is gated.
12. **Profile: full.** Three execution paths.

## Dependencies

The packages below are the direct dependencies: the ones whose entry points the three execution paths call first. The suite as a whole exercises every package from 01 to 54 and is implemented last, after all of them; a scenario cannot pass before the feature it proves exists.

- **09 cli_foundation** (draft): the `parley` binary's client, output and exit codes, which every CLI step of a scenario drives. Added to the plan's list by the brief's rule for packages that use the CLI.
- **11 workspace_read_update_settings** (draft): `GET /workspaces/:slug`, which scenarios read and wait on. Added to the plan's list by the same rule.
- **25 optimistic_landing** (draft): the land route and attempt that the landing, conflict, policy and revert scenarios drive.
- **15 task_fork_creation** (draft): the fork-create call with its token, the first step of the agent's journey.
- **21 scm_write_api** (draft): commit creation and ref moves without a clone.
- **30 awareness** (draft): activity, overlaps and claims.
- **52 durable_event_log_and_stream** (draft): the event log and stream every scenario reads to observe what the hub did.
- External: stock `git` 2.38 or later on the machine that runs the suite; for the sandbox scenarios an OpenShell gateway and the reference sandbox image.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Tracking branch**: In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token. Nobody can force-push or delete it. By default the default branch.
- **Direct push**: A fast-forward update of a protected branch by the workspace's owner or the admin token, over git or the source control API, without a change.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Stack**: Changes linked by `depends_on`, lowest first.
- **Restack**: The hub rebasing a dependent change onto the new head of the change below it, or onto the target once that change has landed.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Land attempt**: One request to land a change (or a stack), with its outcome.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Policy**: A workspace's ordered rules that map touched paths to landing requirements.
- **Approval**: A user's verdict (`approve` or `reject`) on one version of a change.
- **Check**: A named result (`pending`, `passed`, `failed`) reported on one version by an external runner.
- **Claim**: An advisory, expiring statement that a session intends to work on some paths.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent.
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Event webhook**: A workspace's subscription that makes the hub send each matching hub event to a URL as a signed HTTP `POST`, in order and with retries.
- **Scenario**: In this package, one named test of the suite that proves one statement of an acceptance criterion.
- **Harness**: In this package, the helpers under `test/acceptance/harness/` that start the hub and its surrounding fakes and run the CLI and git.
