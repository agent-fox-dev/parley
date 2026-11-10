---
spec_id: "54"
spec_name: "operations_and_packaging"
title: "Run, back up and package the hub"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "f1e79c9b9c9359104a2f8fed3db62b89ae79c91cdd986f02b37be489b51b43d5"
schema_version: 3
source: "docs/prd/15_non_functional.md"
---

# Run, back up and package the hub

## Intent

An operator must be able to trust the hub as a thing that is run: it refuses to start on a host that cannot serve git, it opens the data an earlier release wrote, its logs can be fed to a machine and never leak a credential, a backup taken while it works restores to a hub that starts, it ships as images and manifests that run it the one supported way, and its performance targets are measured rather than assumed. This package delivers those guarantees of the non-functional requirements that no feature package owns.

## Goals

- `parleyd` runs `git --version` once at every serving start and refuses to start (exit status 1, one error line) when `git` is missing, older than 2.38 or does not answer.
- A recorded data directory of each release is kept in the repository, and a test starts the current binary on each one: the missing schema steps are applied, the recorded ones are kept, the repositories are still read by stock `git`, and a second start applies nothing.
- Every log line is one JSON object with `time`, `level` and `msg`; a line written while a request is handled carries that request's `request_id`; no attribute of any type and no query string carries a credential into the log.
- A snapshot written by `parleyd backup` while writers commit transactions and git clients push is copied to a new place and a hub started on the copy starts, answers `GET /readyz` with 200 and accepts new work; its stores pass an integrity check and its repositories pass `git fsck`.
- A hub container image runs `parleyd` as a non-root user with its configuration and data on one volume; a reference sandbox image holds `git` and the `parley` CLI; Kubernetes manifests run the hub image with exactly one replica on a persistent volume.
- `go test ./test/perf/` with `PARLEY_PERF_URL` set measures the fourteen targets of §15.4 against a running hub and writes a JSON report that says, per target, the measured value and whether the target was met. Without that variable the test is skipped, and a missed target never fails a test run.

## Non-goals

- The `parleyd backup` command itself, the stores, the mechanism of schema steps (registration, order, one transaction per step, the record of applied names), the scrubbing function, the logger's level filter and the JSON-lines output (NFR-001, NFR-002, the mechanism of NFR-003, NFR-007's command, NFR-021, NFR-043): package 03 server_lifecycle_and_configuration. This package tests and extends them and does not specify them again.
- The access-log line and the request id (PLAT-FR-018): package 02 http_api_conventions. That the line's `path` holds no query string is package 02's criterion too (REQ-8.5 there); this package relies on it and changes nothing in `internal/server/accesslog.go` (Design Decision 20). The level names of Design Decision 8 are written by package 03's logger in `internal/hub/logging.go`, which this package extends.
- The two binaries, the build values set by the linker and their report by `GET /version` and `parley version` (NFR-005, the binaries of NFR-006, NFR-041): package 01 hub_skeleton.
- No lock that a request waits on, and compare-and-swap of refs (NFR-010): packages 18 git_push_rules, 21 scm_write_api and 25 optimistic_landing. The exclusive periods of clone, reclone, archive and delete: packages 10 workspace_create_and_clone and 14 workspace_archive_reclone_delete.
- Readers never see a partial update (NFR-011) and state that must agree changes atomically (NFR-012): packages 18 git_push_rules, 22 change_opening_and_versions, 25 optimistic_landing, 34 rebuild_execution and 52 durable_event_log_and_stream.
- Background work survives a restart, and refs are reconciled with records at start (NFR-013): packages 10 workspace_create_and_clone, 23 change_lifecycle_and_mergeability, 24 change_stacks_and_restack, 25 optimistic_landing, 29 auto_land_revert_and_path_limits, 33 rebuild_jobs and 45 sandbox_creation_and_credentials, each as a start hook of package 03. This package shows only that those hooks run on restored data.
- What is lost at a restart without harm (NFR-014): packages 02 http_api_conventions (rate-limit counters), 52 durable_event_log_and_stream (streams) and 47 sandbox_lifecycle_and_checkpoints (SBX-FR-060).
- Security requirements NFR-020 to NFR-028 and NFR-044: packages 39 secrets and 06 api_keys_and_access_tokens (NFR-020), 03 (NFR-021), 07 workspace_access_rule and 17 git_smart_http (NFR-022), 17 and 18 (NFR-023), 06 and 08 workspace_bound_tokens (NFR-024), 43 forge_webhook_ingress (NFR-025), 18, 21 and 25 (NFR-026), 45 sandbox_creation_and_credentials and 48 sandbox_network_policy (NFR-027, NFR-028), 53 event_webhooks (NFR-044).
- The limits of §15.5: each is specified by the package of the section the table names.
- The metrics catalogue and `GET /metrics` (NFR-040): package 51 audit_query_and_retention.
- The behaviour the performance scenarios call (forks, the source control API, changes, landing, events, sandboxes): packages 15, 20, 21, 22, 23, 25, 52, 45 and 46. The harness is a client of their routes and specifies none of them.
- Black-box acceptance of the product: package 55 acceptance_suite.
- Not covered by this package: opening the data of a newer release with an older binary (the PRD requires only the other direction); a `restore` command (the PRD defines none; restoring is copying files); a bootstrap manifest for Kubernetes (NFR-006 lists none, see Design Decision 14); a registry, a release pipeline or image signing; a Helm chart; failing a build on a missed performance target (§15.4 calls the targets an initial bar to be revisited with measurements).

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/15_non_functional.md`. Most of its requirements restate guarantees that feature sections own; the Non-goals name the owner of each. What is left for this package is the part of §15 that is about running the product:

- NFR-004: "The server needs the `git` command at run time (version 2.38 or later, checked at start)."
- NFR-003: "a newer binary can always open the data of the previous release."
- NFR-042: "Logs are structured (one JSON object per line), carry the request id, and never contain credentials or secret values."
- NFR-007: "A hub started on a restored snapshot starts cleanly."
- NFR-006: a hub container image, a reference sandbox image that satisfies the image contract of §13.9, and Kubernetes manifests.
- §15.4: the performance targets, "the initial bar ... to be revisited with measurements".

Package 03 delivers what this package builds on: the run function of the hub in `internal/hub/` (it takes the arguments, an environment lookup, the working directory, stdout and stderr and returns the exit status), start hooks that run in registration order before the listener is bound, the rule that a failing start exits 1 with one JSON error line, the stores with their schema steps in `internal/store/`, the logger in `internal/hub/logging.go` that applies the scrubber of `internal/scrub/`, and `parleyd backup`. Package 02 delivers the request id and the access-log line in `internal/server/`.

This package builds in these places:

| Path | Holds |
|---|---|
| `internal/gitrepo/version.go` | reading and comparing the version of the `git` command |
| `internal/hub/` | the registration of the git check as the first start hook; the logging rules in `logging.go` |
| `test/upgrade/` | the recorder (`test/upgrade/record/`), the recorded archives (`test/upgrade/testdata/`), the compatibility rule and the upgrade test |
| `test/e2e/` | the restore test and the log audit |
| `containers/hub/Dockerfile`, `containers/sandbox/Dockerfile` | the two images |
| `test/image/` | the image tests, run only when `PARLEY_IMAGE_TESTS=1` |
| `deploy/kubernetes/` | the manifests and their test |
| `test/perf/` | the performance harness |

## Requirements

### The `git` version check at start (NFR-004)

When `parleyd` starts to serve, it runs `git --version` once. The check is the first start hook, named `git-version`: it runs after the configuration is loaded, the data is opened and the bootstrap has run, and before every other start hook, so no hook ever runs `git` on a host where the check would fail. `git` is looked up on the `PATH` of the environment the hub was started with.

The first line of the output must have the form `git version <major>.<minor>` followed by anything: `git version 2.38.0`, `git version 2.39.3 (Apple Git-146)`, `git version 2.45.1.windows.1` and `git version 2.50.0-rc1` are all read. The version is accepted when (major, minor) is at least (2, 38): 2.38.0 is accepted, 2.37.7 is refused, 3.0 is accepted. On success the hub writes one info log line with `msg` `git version checked` and the member `git_version` holding the text after `git version ` up to the first space.

Failures stop the start as every failing start hook does (exit status 1, one JSON error line on stderr, no `listening` line, no later hook called). The error line's text contains:

| Case | Text |
|---|---|
| no `git` on `PATH` | `git not found on PATH; parleyd needs git 2.38 or later` |
| version below 2.38 | `git <found> is too old; parleyd needs git 2.38 or later` |
| first line not of the form above | `cannot read the git version from "<first line, cut to 100 bytes>"` |
| `git --version` exits with a status other than 0 | `git --version failed: exit status <n>` |
| no answer within 10 seconds | `git --version failed: no answer within 10s`; the `git` process is killed |

Runs that do not serve do not run `git`: `parleyd --help`, a usage error, `parleyd backup <directory>`, and a bootstrap run that writes the admin token and exits (`--admin-email` on a new hub, `--reset-admin-token`). They behave the same on a host without `git`.

### Opening the previous release's data (NFR-003)

**Recorded data.** `go run ./test/upgrade/record <name>` records the data of the binary it is built from. It starts a hub in a temporary directory with `--admin-email`, starts it again to serve, creates one bare repository `fixture.git` with one commit on `main` under the workspace root, stops the hub with a clean shutdown, and writes `test/upgrade/testdata/<name>.tar.gz`. The archive holds the data directory (the transactional store, the analytical store, the workspace root) and `manifest.json`:

```json
{"release":"<name>","recorded_at":"<timestamp>",
 "steps":{"transactional":["<step>", "..."],"analytical":["<step>", "..."]},
 "admin_token_sha256":"<64 hex>",
 "repositories":[{"path":"fixture.git","refs":{"refs/heads/main":"<40 hex>"}}]}
```

`steps` lists the schema steps each store records as applied, in the order they were applied. `<name>` matches `^[a-z0-9][a-z0-9._-]{0,62}$`. The recorder exits 0 and prints the archive's path. It exits 1, with one line on stderr and nothing written or changed, when the name does not match or the archive already exists. The archive `baseline.tar.gz` is recorded when this package is implemented; one archive is recorded and committed at each release, named after the release. Archives older than the previous release may be deleted.

**The guarantee.** For every archive under `test/upgrade/testdata/`, a hub started on the unpacked data with no flags:

- applies every registered schema step the data does not record and no recorded one, logs the `listening` line and answers `GET /readyz` with 200;
- records, in each store, a list of applied steps that begins with the manifest's list for that store in the same order;
- still holds the admin token hash of the manifest;
- leaves each repository readable by stock `git`: `git fsck` succeeds and each ref of the manifest resolves to the commit id of the manifest;
- stops with exit status 0 on SIGTERM, and when started again applies no step and leaves both lists of applied steps as they were.

**The compatibility rule.** Data can be opened only if the binary still knows what the data records. A list of recorded steps is compatible with a list of registered steps when every recorded name is registered and the recorded names appear among the registered ones in the same relative order. The upgrade test applies this rule to every archive before it starts the hub and fails naming the first step that breaks it; so renaming, removing or reordering a released schema step fails the ordinary test run.

### Structured logging (NFR-042)

Package 03 writes every line as one JSON object on stderr, filters by level and passes the message and string attributes through the scrubber. This package fixes the rest.

**Members of every line.** `time` is an RFC 3339 timestamp in UTC ending in `Z`. `level` is one of `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`. `msg` is a string. A line is exactly one line: a line break inside a message or a value appears escaped.

**The request id.** A line logged with the context of a request carries the member `request_id`, equal to the `X-Request-ID` header of that request's response. This holds for lines written by a handler, by a middleware and by code a handler calls with the request's context. A line logged outside a request (start, start hooks, the maintenance loop, background work) has no `request_id` member.

**Values of every type are scrubbed.** An attribute whose value is an error, a nested group of attributes, a list, a map, a byte slice or any value that is rendered as text is scrubbed like a string attribute: after rendering, no string in the line holds a registered secret value, a hub credential or user information in a URL. An error is rendered as its message. Numbers, booleans and durations are written unchanged.

**No query string.** The `path` member of the access-log line is the path of the request's URL without the query string, so a token passed as a query parameter (the forge ingress accepts one, §12) does not reach the log.

**A failing log write does not fail the hub.** When a write to stderr returns an error, the line is dropped; the request that caused it is answered as it would have been and the process keeps running.

**The audit.** A hub that runs at level `trace` and receives requests that carry marker secrets in an `Authorization: Bearer` header, in an `Authorization: Basic` header, in a `Cookie` header, in a JSON body and in a query parameter writes a log in which every line has the three members above, every access-log line has the `request_id` of its response, and no marker appears.

### Restoring a backup taken under load (NFR-007)

**Restoring** is copying: the operator copies the snapshot's transactional store to `[database].path`, its `analytics.db` beside it and its workspace directory to `[workspace].path` on the new host, puts the configuration file in place and starts `parleyd` with no flags. The snapshot holds no configuration directory; the configuration file is the operator's to keep.

A hub started on such a copy starts cleanly: it applies no schema step, runs every registered start hook to completion with the restored data open, logs the `listening` line and answers `GET /readyz` with 200. It still holds the admin token hash of the source hub. It accepts new work: a transaction commits and a push to a restored repository is accepted by stock `git`. The snapshot directory that was copied from is unchanged by all of this.

This holds for any snapshot taken under load, where load means at least four writers committing transactions to the transactional store, one writer committing to the analytical store and two git clients pushing new commits to two repositories under the workspace root for the whole duration of the backup:

- during the backup no writer's commit and no push returns an error;
- `PRAGMA integrity_check` answers `ok` on each restored store;
- `git fsck` succeeds on each restored repository, and each of its refs resolves to a commit that is the source repository's commit of that ref at the end of the load or an ancestor of it;
- every commit that was pushed before the backup started is reachable from the restored ref.

### The hub container image (NFR-006)

`containers/hub/Dockerfile`, built with the repository root as its context and the build arguments `VERSION`, `COMMIT` and `BUILD_TIME`, gives an image that:

- contains `parleyd` and `parley` in `/usr/local/bin`, both built without cgo and with the three build values set, and a `git` of version 2.38 or later;
- runs as the user and group id 10001, never as root;
- sets `XDG_CONFIG_HOME=/var/lib/parley/config` and `XDG_DATA_HOME=/var/lib/parley/data`, declares `/var/lib/parley` as a volume and uses it as working directory, so that the configuration file, the admin token file, both stores and the repositories live on one volume;
- declares port 8080 and has the entry point `parleyd` in exec form, so that arguments of the container are arguments of `parleyd` and a stop signal reaches it.

A container started from the image with no arguments and an empty volume listens on port 8080, answers `GET /readyz` with 200, reports the build argument `VERSION` in `GET /version`, and has created `data/parley.db` on the volume as user 10001. When the container is stopped, `parleyd` exits with status 0 within 30 seconds. `parley version` run in the image prints the same `VERSION` as the client's version. `backup /var/lib/parley/snapshot` as the container's arguments, on the volume of a running hub, writes a snapshot and exits 0.

### The reference sandbox image (NFR-006, §13.9 image contract)

The image contract of §13.9: "A profile's image must contain `git` for the checkout and for checkpoints. Nothing else is required: no agent, no Parley component. An image that also contains the `parley` CLI can use it as it is."

`containers/sandbox/Dockerfile`, built like the hub image, gives the reference image. It contains `git` (2.38 or later), a POSIX shell at `/bin/sh`, the CA certificates of its base image and the `parley` CLI in `/usr/local/bin`. It contains no agent and no `parleyd`. It has no entry point, so the command the runtime starts runs unchanged, and its default user is not root (user id 1000, with a writable home directory).

The image tests hold a **contract check** that takes any image reference: as the image's default user, in a writable directory, `git init`, a commit and `git rev-parse HEAD` succeed. The tests run it against the reference image and, when `PARLEY_SANDBOX_IMAGE` is set, against that image too, so an operator can check an image of their own.

**Running the image tests.** The tests in `test/image/` need a container engine. They are skipped unless `PARLEY_IMAGE_TESTS=1`; the engine is the command named by `PARLEY_CONTAINER_ENGINE`, default `docker`. With `PARLEY_IMAGE_TESTS=1` and no such command they fail naming the command.

### Kubernetes manifests (NFR-006, NFR-001)

`deploy/kubernetes/` holds five files, every object named `parley` unless said otherwise and labelled `app.kubernetes.io/name: parley`:

| File | Object |
|---|---|
| `configmap.yaml` | a ConfigMap with one key, `config.toml` |
| `secret.example.yaml` | a Secret `parley-secrets` whose every value is the placeholder `replace-me` |
| `pvc.yaml` | a PersistentVolumeClaim `parley-data`, access mode `ReadWriteOnce`, 20Gi, no storage class named |
| `deployment.yaml` | a Deployment running the hub image |
| `service.yaml` | a Service of type `ClusterIP`, port 8080 to the container's port 8080 |

- **One process.** The Deployment has `replicas: 1` and the strategy `Recreate`, so that an update stops the old hub before it starts the new one and two hubs never open one data directory (NFR-001).
- **Storage.** The claim is mounted at `/var/lib/parley`. The ConfigMap's `config.toml` is mounted as the single file `/var/lib/parley/config/config.toml` (a `subPath` mount), which leaves the config dir on the volume and writable for the admin token file. The pod runs as user 10001, refuses to run as root, and sets `fsGroup: 10001`.
- **Configuration and secrets.** The Secret reaches the container as environment variables (`envFrom`). The `config.toml` of the ConfigMap is accepted by the hub's configuration loader when the environment holds the keys of the example Secret, and every `${NAME}` it uses is a key of the example Secret. It shows one OAuth provider whose `client_secret` is `${GITHUB_CLIENT_SECRET}`.
- **Probes, port and stop.** The container's liveness probe is `GET /healthz` and its readiness probe `GET /readyz`, both on port 8080. The Service selects the pod's labels. `terminationGracePeriodSeconds` is 40, above the 30 seconds within which the hub exits after SIGTERM (PLAT-FR-004).

### The performance harness (§15.4)

**Running it.** `test/perf/` holds a harness that is a client of one running hub. The operator runs:

```sh
PARLEY_PERF_URL=https://hub.example PARLEY_PERF_TOKEN=... PARLEY_PERF_WORKSPACE=acme-app \
  go test ./test/perf/ -run TestPerformanceTargets -count=1 -timeout 3h -v
```

| Variable | Meaning |
|---|---|
| `PARLEY_PERF_URL` | the hub's base URL. Unset: `TestPerformanceTargets` is skipped, with a message naming the variable |
| `PARLEY_PERF_TOKEN` | a user's credential with write access to the workspaces |
| `PARLEY_PERF_WORKSPACE` | one or more slugs of cloned workspaces, separated by commas. The first is used by every scenario; the others only by `active_forks` |
| `PARLEY_PERF_SANDBOX_PROFILE` | a sandbox profile. Unset: the three sandbox scenarios are `skipped` |
| `PARLEY_PERF_CAPACITY` | `1` runs the four capacity scenarios. Otherwise they are `skipped` |
| `PARLEY_PERF_REPORT` | where the report is written; default `parley-perf-report.json` in the system's temporary directory |

In the ordinary test run (`go test ./... -count=1`) no variable is set and the test is skipped; the harness's own logic (percentiles, verdicts, the report, the scenario runner) is tested there against a stub hub.

With `PARLEY_PERF_URL` set, the test fails only when the harness cannot do its job: `PARLEY_PERF_TOKEN` or `PARLEY_PERF_WORKSPACE` is unset (the message names the variable), `GET /version` does not answer 200 within 10 seconds, or the report cannot be written. A target that is missed, a scenario that is skipped and a scenario that ends in an error never fail the test.

**Scenarios.** Fourteen, one per row of §15.4, run one after the other in this order:

| Name | What is done | Measured | Target |
|---|---|---|---|
| `fork_create` | 50 forks created one after the other (`POST /workspaces/:slug/forks`) | p95 of the request duration, ms | under 1000 |
| `file_read` | 200 reads, alternately one file and one tree (`GET …/scm/file`, `GET …/scm/tree`) | p95, ms | under 100 |
| `commit_create` | 50 commits of 100 files each on a fork (`POST …/scm/commits`) | p95, ms | under 500 |
| `land` | 20 changes of 20 commits each with disjoint files, landed one at a time | p95 of the time from the land request to the land attempt's success, ms | under 2000 |
| `land_throughput` | 120 one-commit changes with disjoint files landed on one target branch, at most 8 land requests in flight | changes landed × 60 ÷ seconds elapsed | at least 60 |
| `land_concurrency` | 16 land requests for 16 non-conflicting changes sent at the same time | land attempts that did not end in success | at most 0 |
| `mergeability` | 100 open changes; the target branch is moved by landing one more change | seconds until none of the 100 reports `mergeable` `unknown` | at most 60 |
| `event_latency` | a subscriber on `GET /events`; 50 commits created one after the other | the largest time from a commit's response to the arrival of its event, ms | at most 1000 |
| `active_forks` (capacity) | forks created over all listed workspaces until 5,000 exist or the hub refuses | forks that exist at once | at least 5000 |
| `open_changes` (capacity) | changes opened in the first workspace until 2,000 are open or the hub refuses | open changes | at least 2000 |
| `event_streams` (capacity) | `GET /events` connections opened until 500 are open or the hub refuses | connections open at once | at least 500 |
| `sandboxes_running` (capacity, sandbox) | sandboxes created until 200 are `provisioning` or `ready` or the hub refuses | sandboxes in those states | at least 200 |
| `sandbox_ready` (sandbox) | 5 sandboxes created one after the other | the largest time from the create response to state `ready`, s | at most 60 |
| `sandbox_exec` (sandbox) | 50 execs of the command `true` in one sandbox | p95 of the request duration, ms; an upper bound of the hub's overhead | under 200 |

p95 is the nearest-rank percentile: of n samples sorted ascending, the one at position ⌈0.95·n⌉. A scenario that collected no sample has no measurement.

**Verdict of a scenario.** `met` when the measured value satisfies the target; `missed` when it does not; `skipped` when its variable is not set (the detail names the variable); `error` when a request that is not the scenario's expected stop is answered with a status of 400 or more or fails in transport, or when the scenario runs past its deadline (10 minutes; 60 minutes for a capacity scenario). The detail of an `error` names the method, the path, the status and the `error_type` of the first failing request, or says `deadline exceeded`. For a capacity scenario a refusal by the hub is the expected stop: the scenario ends, the count reached is the measurement and the detail holds the refusal's `error_type`. A scenario that ends in `error` does not stop the run; the next scenario runs.

**The harness leaves no work behind.** It never targets a workspace's default branch: it creates a target branch `perf/<run id>` and forks of its own. When a scenario ends, in any verdict, the harness deletes the forks and sandboxes that scenario created and closes its connections; a clean-up request that fails is listed in the report and changes no verdict.

**The report.** One JSON document, written to the report path and logged as a table by the test:

```json
{"hub":{"url":"https://hub.example","version":{"version":"1.4.0","commit":"3f2a1bc"}},
 "run_id":"<12 hex>","started_at":"<timestamp>","finished_at":"<timestamp>",
 "results":[{"name":"fork_create","target":{"comparison":"lt","value":1000,"unit":"ms"},
   "measured":412.5,"samples":50,"status":"met","detail":""}],
 "cleanup_errors":[]}
```

`results` holds exactly fourteen entries in the order of the table, whatever happened. `comparison` is `lt`, `le` or `ge`. `measured` is `null` when there is no measurement. `hub.version` is the document `GET /version` returned. The token appears nowhere in the report, and text taken from a hub response is cut to 200 bytes.

### End-to-end flows

**A host with an old git.** An operator starts `parleyd` on a host whose `git` is 2.30. `parleyd` loads its configuration, opens its data, runs `git --version`, writes one error line saying that git 2.30 is too old and that 2.38 is needed, and exits 1 without listening. The operator installs a newer `git` and starts `parleyd` again; it logs `git version checked`, runs the other start hooks, listens, and answers `GET /readyz` with 200.

**An upgrade.** An operator stops the hub of the previous release and starts the new `parleyd` on the same data. `parleyd` opens the stores, applies the schema steps the data does not record, runs its start hooks and listens. The operator reads `GET /readyz` and receives 200; the repositories are as they were. At the next restart `parleyd` applies no step.

**Backup under load, then restore.** While agents commit and push, an operator runs `parleyd backup <directory>`; `parleyd` writes the snapshot and the work of the agents goes on without an error. The operator copies the snapshot to a new host and starts `parleyd` there; it applies no step, runs its start hooks on the restored data, listens and answers `GET /readyz` with 200. The stores pass their integrity check and stock `git` reads every repository.

**A performance run.** An operator sets `PARLEY_PERF_URL`, the token and the workspace and runs the performance test. The harness reads `GET /version` from `parleyd`, runs the fourteen scenarios against it one after the other, deletes what it created, and writes the report. The operator reads, per target, the measured value and the verdict.

## Design Decisions

1. **Project decisions relied on.** Go; the server needs the `git` command, 2.38 or later, run as a subprocess; the stores are embedded SQLite through `modernc.org/sqlite` behind `internal/store`; HTTP is the standard library's; tests use the standard `testing` package, table-driven, with `net/http/httptest` and `t.TempDir()`; `test/e2e` tests start a hub in-process through the run function of `internal/hub`.
2. **The git check is the first start hook.** The PRD says "checked at start" and not where. Package 03's start hooks already give a failing start its exit status and its one error line, so the check is a hook named `git-version` that the hub's composition registers before every other. A consequence: runs that exit before the hooks (`--help`, `backup`, the bootstrap runs) do not check, and need no `git`.
3. **Only major and minor are compared**, and whatever follows them is ignored, because vendors append their own suffixes. The time limit is 10 seconds. `git` is found on the `PATH` of the hub's environment lookup, which lets a test place a script named `git` first on that `PATH`.
4. **Recorded data is an archive, not a directory.** A bare repository holds empty directories that git does not track, so a recorded data directory is kept as one `.tar.gz` per release and unpacked by the test.
5. **What a recording holds.** A bootstrapped, cleanly stopped hub with one repository. The PRD does not say what "the data of the previous release" must contain; this is the smallest data that has every table of the release, an admin token hash and a repository. Seeding a recording with workspaces, changes and sessions through the REST API is later work.
6. **Every recorded archive is tested, not only the previous release's.** The PRD requires the previous release; keeping older archives passing costs nothing until one fails, and then deleting it is a visible decision.
7. **The compatibility rule is a function of the upgrade package**, so that a renamed or removed step fails with the step's name instead of with whatever the step would do when applied a second time. Reading a store's applied steps is added to `internal/store` as a read-only listing.
8. **Level names.** `TRACE`, `DEBUG`, `INFO`, `WARN`, `ERROR`, the spelling package 03's tests already expect for `ERROR`. `time` is UTC so that logs of hosts in different zones sort together.
9. **The request id reaches a line through the context.** Code logs with the request's context; the logger adds `request_id` from the id package 02 puts there. Code that logs without a context gets no id; the PRD's "carry the request id" is read as "where there is a request".
10. **The query string is left out of the access log** altogether instead of being scrubbed, because a value in it that the scrubber does not know (a forge webhook token) would pass.
11. **Restore is a copy, not a command.** The PRD defines `parleyd backup` and no counterpart. The two stores of a snapshot are copied one after the other, so they need not show the same instant; a hub starts on them regardless, and the feature packages' start hooks reconcile.
12. **Images.** The base image is the implementer's choice among those whose packaged `git` is 2.38 or later; the hub image test fails otherwise, because the hub in it refuses to start. User id 10001 for the hub and 1000 for the sandbox image, port 8080, the volume `/var/lib/parley` and the image names `parley-hub` and `parley-sandbox` are this package's choices; the PRD names none.
13. **The config dir is on the data volume.** The bootstrap writes the admin token file into the config dir and the hub refuses to start while the file exists (PLAT-FR-003). A read-only ConfigMap directory cannot hold that file and a container's own filesystem loses it at the next restart, so the image puts the config dir on the volume and the manifests mount only the file `config.toml` into it.
14. **No bootstrap manifest (a known gap of the source PRD).** The Deployment runs `parleyd` with no flags; a hub that is not bootstrapped serves without an admin token. How an operator runs `parleyd --admin-email` once on the volume and takes the token file is a cluster procedure NFR-006 does not list; it is left out. The gap is recorded here and no behaviour is added for it.
15. **`Recreate`, one replica, 40 seconds.** A rolling update would start a second hub on the same data, which NFR-001 forbids. 40 seconds is the hub's 30-second bound plus a margin.
16. **Image tests and the performance test are gated by environment variables, not build tags**, so they are compiled and linted in every run and skipped unless asked for. Manifests are checked by parsing them; no cluster is needed.
17. **The harness is a black-box HTTP client of a hub the operator names.** The targets are stated for a host of 8 cores and 32 GB with repositories of up to 5 GB; only the operator can provide that hub and that workspace. The harness uses `net/http` directly and imports no server code. Its exact requests are those of the packages that own the routes.
18. **Samples and deadlines** (50, 200, 20, 120, 16, 100, 5; 10 and 60 minutes) are this package's choices. `sandbox_exec` reports the whole request duration of `true` as an upper bound of the overhead, because a client cannot separate the hub's share. `event_latency` and `sandbox_ready` report the largest sample, since their targets say "within".
19. **Capacity scenarios stop at the hub's refusal and report the count (a known gap of the source PRD).** The default limits (200 forks per workspace, 20 sandboxes per user) are below two of the capacity targets; whether the operator lists 25 workspaces or raises a limit is theirs to decide, and the report shows what was reached. The source PRD's defaults and targets disagree; this package changes neither.
20. **Package 02 is added to the dependencies** beside the plan's 03: the request id a log line must carry is created there, and the access-log line is written by its `internal/server/accesslog.go`. Dropping the query string from `path` is package 02's criterion (REQ-8.5 there: `path` is the URL path without the query string); this package relies on package 02 for that point, edits nothing in that file, and only confirms it in its own log tests.
21. **Profile: full**, with four execution paths.

## Dependencies

- Package 03 server_lifecycle_and_configuration (draft): the run function, start hooks, the stores and schema steps, the logger and scrubber, `parleyd backup`, the configuration loader.
- Package 02 http_api_conventions (draft): the request id in the request's context and the access-log line.
- External: a container engine (`docker` or a compatible command) for the image tests only; a running hub for the performance test only.

## Verified External API

`gopkg.in/yaml.v3` v3.0.1, read in the module cache (`yaml.go`); used only by the manifest test:

- `func Unmarshal(in []byte, out interface{}) (err error)`
- `func NewDecoder(r io.Reader) *Decoder`
- `func (dec *Decoder) Decode(v interface{}) (err error)`

`log/slog`, `archive/tar`, `compress/gzip` and `os/exec` are the standard library's.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Land attempt**: One request to land a change (or a stack), with its outcome.
- **Sandbox**: An isolated environment in which an agent runs, attached to a workspace (normally a task fork) and started with it checked out. It runs on OpenShell; the hub controls it (§13).
- **Profile**: The operator's template for a sandbox, defined in configuration: image, resources, time limits, base policy, credentials and the secrets it exposes.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Snapshot**: The directory `parleyd backup` writes: the two stores and the workspace root.
- **Recorded data**: An archive under `test/upgrade/testdata/` holding the data directory a release wrote, with its manifest.
