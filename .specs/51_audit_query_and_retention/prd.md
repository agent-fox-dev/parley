---
spec_id: "51"
spec_name: "audit_query_and_retention"
title: "Query the audit record, prune it and export metrics"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "3484736ef0c679a1535d0cb3b01a37ef053b4bf1256fcadb5d8e7afc2f478f80"
schema_version: 3
source: "docs/prd/14_sessions_audit_events.md"
---

# Query the audit record, prune it and export metrics

## Intent

An operator must be able to reconstruct who did what on the hub, a developer must be able to inspect what one agent run did, and neither record may grow without bound or go unwatched. This package gives the hub one query over hub events and agent run events, the reads of one run's telemetry, a retention run that keeps every audit and session record within configured limits, and a Prometheus endpoint that always exports every documented metric.

## Goals

- `GET /audit` answers one list that merges hub events and run events, newest first, under the visibility rule for hub events and the readable set for run events, with filters that are validated and never silently ignored.
- Seven per-run read routes under `/workspaces/:slug/runs/:run_id` serve a run's events, session outcomes, tool calls, tool errors, traces, postmortem and a transcript per node, confined to the workspace and run of the path.
- `parley audit query` calls `GET /audit` with one flag per filter and prints the answer.
- Seven configuration keys under `[audit]` and `[sessions]` set the retention limits; a malformed or out-of-range value stops the hub at start with a message naming the key.
- A retention run executes eleven named steps in a fixed order at start and every hour; a failing step is logged and counted and never stops the others; the time of the last fully successful run is exported.
- `GET /metrics` answers the Prometheus text format without a credential and exports every one of the 27 metrics of the catalogue from start, each with at least one series, with every closed label value present at 0.
- Other packages report to one recorder in `internal/metrics` and never write exposition text themselves.

## Non-goals

- Sessions, usage reports, the cost report and `parley session …`: package 49 `agent_sessions_and_usage`. This package only times out and deletes session records in its retention steps.
- The ingestion routes for run telemetry, batch handling, the write timeout `[audit].write_timeout` and the sandbox exec record: package 50 `telemetry_ingestion`. This package reads the records that package stores.
- The hub event record, writing events, the visibility rule itself, `GET /events`, `GET /events/log`, cursor expiry, `parley events tail|list` and the keys `[events].retention` and `[events].max_sse_connections`: package 52 `durable_event_log_and_stream`. This package calls its visibility rule (`Visible`), its expire operation (`Expire`) and its store, and states the rule that `[events].retention` is at most `[audit].max_age`, because it owns the second key.
- Event webhook subscriptions, deliveries and the `[webhooks]` keys: package 53 `event_webhooks`. This package runs the `webhook_deliveries` retention step through the `DeliveryPruner` interface of `internal/audit`, with which package 53 registers itself; this package does not depend on package 53.
- The maintenance loop, its hourly schedule, the configuration loader and its key registry: package 03 `server_lifecycle_and_configuration`. This package registers one task and seven keys.
- The error envelope, list paging, the check order and the rate-limit exemption of `/metrics`: package 02 `http_api_conventions`. The access rule and the readable set: package 07 `workspace_access_rule`. The binding guard for bound tokens on `/workspaces/:slug/…` routes: package 08 `workspace_bound_tokens`.
- The CLI's client, output and exit codes: package 09 `cli_foundation`.
- Deciding when each feature metric changes (`parley_git_requests_total`, `parley_scm_requests_total`, `parley_changes_open`, the land, fork, event, webhook, session, token, job-queue and sandbox metrics): the package that owns the feature (17, 18, 20, 21, 22, 25, 16, 52, 53, 49, 50, 33, 44 to 48). Each reports to the recorder of this package, registers a source with it, or exports a value this package connects in `internal/hub`; the table of the Metrics section names the one reporter of every metric. This package exports the families and counts only HTTP requests and its own retention.
- Structured logging and the performance targets: package 54 `operations_and_packaging`.
- Not covered by this package: a CLI for the per-run reads. The source PRD defines none.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/14_sessions_audit_events.md`: AUD-FR-030 to AUD-FR-034 (reads), AUD-FR-060 and AUD-FR-061 (retention and metrics), §14.7.2 (retention steps), §14.7.3 (the metric catalogue), the `GET` routes under `/workspaces/:slug/runs/:run_id`, `GET /audit` and `GET /metrics` of §14.5, `parley audit query` of §14.6, the keys of §14.9 that set retention limits and the cases of §14.10 that concern these. `docs/prd/03_platform.md` lists `GET /metrics` as a public route outside `/api/v1` (PLAT-FR-010) and `docs/prd/15_non_functional.md` NFR-040 repeats that every documented metric is exported.

The code of this package lives in:

| Path | Holds |
|---|---|
| `internal/metrics/` | the catalogue, the registry, the recorder, sources, the text exposition, the `/metrics` handler and the HTTP request instrumentation |
| `internal/telemetry/read.go`, `postmortem_read.go`, `transcript.go` | the per-run read routes, beside the ingestion code of package 50 |
| `internal/audit/` | the audit query (`query.go`, `filters.go`), the retention configuration (`config.go`), the retention run (`retention.go`) and its steps (`steps_*.go`) |
| `internal/session/retention.go`, `internal/telemetry/retention.go`, `internal/event/retention.go` | the store operations the retention steps call, added beside the stores of packages 49, 50 and 52 |
| `internal/cli/audit.go` | `parley audit query` |
| `internal/hub/` | wiring: the routes, the `retention` maintenance task, the keys, the metrics handler and the instrumentation |
| `test/e2e/audit_test.go`, `test/e2e/metrics_test.go` | end-to-end tests |

Tests live beside the code (`internal/audit/query_test.go`, `internal/metrics/registry_test.go`).

## Requirements

### Per-run record lists (AUD-FR-030, AUD-FR-031, AUD-FR-034)

Five routes list the records of one run: `GET /workspaces/:slug/runs/:run_id/events`, `…/sessions/outcomes`, `…/tools/calls`, `…/tools/errors` and `…/traces`. Each needs read access to the workspace, whatever the workspace's status (an archived workspace is read like any other), and, for a PAT, the scope `audit:read` or `audit:write`. Without a valid credential the answer is HTTP 401 `unauthenticated`; a PAT without either scope gets 403 `insufficient_scope`; a caller without read access, or a slug that does not exist, gets 404 `not_found`. The same holds for the postmortem and transcript routes below.

The answer is 200 with the list envelope. Items carry every field of the record (§14.3.4). They are ordered by (`timestamp`, `id`) ascending; `order=desc` reverses both. Results are always confined to the workspace and the run of the path: a record of another run, or of the same run id in another workspace, never appears. A run without records answers an empty list.

Filters are exact matches and combine with AND:

| Route | Filters |
|---|---|
| `…/events` | `event_type`, `severity`, `node_id`, `session_id` |
| `…/sessions/outcomes` | `node_id`, `session_id`, `status` |
| `…/tools/calls` | `node_id`, `session_id`, `tool_name` |
| `…/tools/errors` | `node_id`, `session_id`, `tool_name` |
| `…/traces` | `event_type`, `node_id`, `session_id` |

All five take `since` (inclusive) and `until` (exclusive) on `timestamp`, `order` (`asc`, `desc`), `limit` and `cursor`.

Every parameter is validated. HTTP 400 `invalid_request` with a message naming the parameter answers: a `run_id` that is not `YYYYMMDD_HHMMSS_xxxxxx` with six lower-case hexadecimal digits; a `severity` outside `info`, `warning`, `error`, `critical`; an `order` outside `asc`, `desc`; on `…/traces` an `event_type` outside the five trace types; a `session_id` that is not a UUID; a `since` or `until` that is not RFC 3339; `since` not before `until`; a `limit` outside 1 to 200; a malformed cursor or one issued for another query. Nothing is ignored or turned into an empty result.

### Postmortem and transcript

`GET /workspaces/:slug/runs/:run_id/postmortem` answers 200 with the postmortem of the run in this workspace, all fields of §14.3.4. When the run has none in this workspace the answer is 404 with `error_type` `postmortem_not_found`.

`GET /workspaces/:slug/runs/:run_id/transcript?node_id=…` rebuilds the conversation of one node from the run's trace records. It takes the traces of the run and node ordered by `timestamp`, then `sequence`. `session.init` becomes role `system`, `assistant.message` becomes `assistant`, `tool.use` becomes `tool_use`, `tool.error` becomes `tool_error`; `session.result` is skipped. `content` is `data.content` when that is a string, else `""`. `tool_name` is `data.tool_name` when that is a string, else null. The answer is 200 `{"run_id", "node_id", "items": [{"role", "content", "tool_name", "timestamp"}], "next_cursor"}`, paged with `limit` and `cursor`. An unknown run or node gives an empty list. A missing `node_id` and a malformed `run_id` answer 400 `invalid_request`.

### The audit query (AUD-FR-032, AUD-FR-033)

`GET /audit` is one query over hub events and run events. It needs a credential and, for a PAT, `audit:read` or `audit:write`.

The answer is 200 with the list envelope, newest first: by time, then `id`, both descending. `since` is inclusive and `until` exclusive, on a hub event's `time` and a run event's `timestamp`.

Every item has the same seventeen members. `source` (`hub` or `agent`), `id`, `type`, `time`, `workspace` and `session_id` are set for both sources. A hub item also has `seq`, `actor_type`, `actor_id`, `resource_type`, `resource_id`, `action` and `metadata` as the hub event record holds them (§14.3.5). An agent item also has `run_id`, `node_id`, `severity` and `payload`; its `type` is the run event's `event_type` and its `time` the run event's `timestamp`, with its fractional seconds. Members of the other source are null.

A caller sees hub events under the visibility rule of §14.3.7 and run events of the workspaces the caller can read. An admin token sees everything, including records of workspaces that no longer exist. A bound token sees only records of the workspaces its binding covers and never a hub event without a workspace. `parley.webhook.ping` never appears.

Filters:

| Parameter | Applies to | Meaning |
|---|---|---|
| `source` | both | `hub` or `agent`. Default: both |
| `workspace` | both | exact slug |
| `types` | both | comma-separated list of at most 20 entries; an entry is an exact type or a prefix ending in `*`; `*` alone matches everything; `*` only as the last character. Entries combine with OR. For a run event the type is its `event_type` |
| `session_id` | both | UUID |
| `since`, `until` | both | inclusive, exclusive |
| `actor_id`, `actor_type`, `resource_type`, `resource_id`, `action` | hub | exact match. Using one excludes run events |
| `run_id`, `node_id`, `severity` | agent | exact match. Using one excludes hub events |
| `limit`, `cursor` | both | list paging |

Filters combine with AND. A type that no record has is not an error; it gives an empty list. A hub-only filter together with an agent-only filter gives 200 with an empty list.

Failures (AUD-FR-034):

- HTTP 400 `invalid_request`, naming the parameter: a `source`, `actor_type` or `severity` outside its set; a `session_id` that is not a UUID; a malformed `run_id`, `since`, `until` or cursor; `since` not before `until`; a `types` list with more than 20 entries, an empty entry or a `*` that is not the last character; a `limit` outside 1 to 200; a hub-only filter with `source=agent`; an agent-only filter with `source=hub`.
- HTTP 404 `not_found` (`workspace not found`): the `workspace` filter names a workspace the caller cannot read, one outside a bound token's binding, or one that does not exist.
- HTTP 401 `unauthenticated` without a valid credential; 403 `insufficient_scope` for a PAT with neither audit scope.

### `parley audit query`

Calls `GET /audit` and prints the response body. Flags: `--source hub|agent`, `--workspace <slug>`, `--types <list>`, `--session <id>`, `--run-id <id>`, `--actor-id <id>`, `--resource-type <t>`, `--resource-id <id>`, `--action <a>`, `--since <time>`, `--until <time>`, `--limit <n>`, `--cursor <c>`. A flag that is not given sends no parameter. Exit status and error output follow the client's conventions: 0 with the JSON on stdout; 1 with the hub's error envelope on stderr when the hub refuses; 2 for a usage error (a positional argument, a `--limit` that is not an integer), before any request.

### Retention configuration (§14.9)

| Key | Default | Range | Effect |
|---|---|---|---|
| `[audit].max_age` | `90d` | ≥ `1d` | hub events, run events, outcomes, tool calls, tool errors |
| `[audit].max_runs` | `50` | ≥ 1 | runs with telemetry kept per workspace |
| `[audit].trace_max_age` | `30d` | ≥ `1d` | trace records |
| `[audit].postmortem_max_age` | `180d` | ≥ `1d` | postmortems |
| `[audit].orphan_retention` | `30d` | ≥ `1d` | grace period for records of deleted workspaces |
| `[sessions].max_age` | `90d` | ≥ `1d` | terminal sessions and their usage records |
| `[sessions].max_active_age` | `7d` | ≥ `1h` | active sessions older than this are timed out |

Durations take a number and a unit (`s`, `m`, `h`, `d`). A value that is malformed or out of range stops the hub at start, with exit status 1 and a message that names the key.

`[events].retention` (package 52's key, range from `1h`) must not exceed `[audit].max_age`: an event must not be deleted while it is still in the event log. When it does, the hub stops at start with exit status 1 and a message that names `events.retention` and `audit.max_age`. Equal values are accepted.

### The retention run (AUD-FR-060, §14.7.2)

Retention runs once at start and then every hour, as the `retention` task of the maintenance loop. Its steps run in the order of the table. A failing step is logged with its name and counted on `parley_retention_errors_total{step}`; the remaining steps still run. `parley_retention_last_run_timestamp_seconds` is set, to the time the run ended, only when every step succeeded.

| Step (`step` label) | Action |
|---|---|
| `stale_sessions` | `active` sessions started before now − `[sessions].max_active_age` become `timeout`, with `ended_at` = now and `error_message` = `session exceeded maximum active age` |
| `run_records` | delete run events, session outcomes, tool calls and tool errors with `timestamp` older than `[audit].max_age` |
| `max_runs` | per workspace, when more than `[audit].max_runs` runs have telemetry, delete the run events, outcomes, tool calls, tool errors and traces of the oldest excess runs. A run's age is its earliest record. Postmortems are kept, and so are sessions |
| `traces` | delete trace records older than `[audit].trace_max_age` |
| `postmortems` | delete postmortems ingested before now − `[audit].postmortem_max_age` |
| `sessions` | delete terminal sessions started before now − `[sessions].max_age`, together with their usage records. Active sessions are never deleted |
| `event_log` | events older than `[events].retention` leave the event log; `oldest_seq` moves forward accordingly |
| `webhook_deliveries` | delete event webhook delivery records whose last attempt is older than 7 days. `pending` records are never deleted |
| `hub_events` | delete hub events older than `[audit].max_age` |
| `orphans` | delete sessions, usage records, telemetry and hub events of workspaces that no longer exist, once they are older than `[audit].orphan_retention`. Records of a deleted task fork count as belonging to its parent while the parent exists, so they are kept under the normal limits |
| `gauges` | recompute `parley_audit_records{kind}` and `parley_agent_sessions_active{workspace}` from the stored data |

The newest event always stays in the event log, whatever its age, so the last `seq` is always known. A workspace without active sessions disappears from `parley_agent_sessions_active` at the `gauges` step.

### Metrics (AUD-FR-061, §14.7.3, NFR-040)

`GET /metrics` answers 200 with `Content-Type: text/plain; version=0.0.4` and the Prometheus text exposition. It needs no credential and is outside `/api/v1`. Label values include workspace slugs, so operators should not expose it to untrusted networks.

It exports every metric of this catalogue. Each one has at least one series from start-up, so a documented metric is never absent. Label values with a closed set are all present from start-up with value 0.

| Metric | Type | Labels | Meaning |
|---|---|---|---|
| `parley_http_requests_total` | counter | `method`, `path` (route template), `status` | every HTTP request, API, git and webhook routes included |
| `parley_http_request_duration_seconds` | histogram | `method`, `path` | buckets 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10 |
| `parley_git_requests_total` | counter | `service` (`info-refs`, `upload-pack`, `receive-pack`), `result` (`ok`, `refused`, `error`) | git protocol requests |
| `parley_scm_requests_total` | counter | `op` (`refs`, `commits`, `tree`, `file`, `diff`, `compare`, `blame`, `search`, `commit`, `ref_create`, `ref_move`, `ref_delete`, `rebase`), `result` (`ok`, `ref_moved`, `refused`, `error`) | source control API requests |
| `parley_changes_open` | gauge | `workspace` | changes in state `draft` or `open` |
| `parley_lands_total` | counter | `result` (`landed`, `conflict`, `blocked`, `failed`) | land requests by final result |
| `parley_land_attempts` | histogram | none | compare-and-swap attempts per land. Buckets 1, 2, 3, 5, 8, 13 |
| `parley_land_duration_seconds` | histogram | none | time from land request to final result. Buckets 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60 |
| `parley_forks_active` | gauge | none | task forks that exist |
| `parley_events_seq` | gauge | none | the last `seq` written |
| `parley_sse_connections` | gauge | none | open event streams |
| `parley_webhook_deliveries_total` | counter | `result` (`ok`, `retry`, `failed`) | event webhook attempts |
| `parley_webhook_queue_depth` | gauge | none | events written and not yet delivered or given up |
| `parley_audit_events_total` | counter | `source` (`hub`, `agent`), `type` | hub events written and run events ingested |
| `parley_agent_sessions_active` | gauge | `workspace` | sessions in state `active` |
| `parley_agent_tokens_total` | counter | `workspace`, `model`, `direction` (`input`, `output`, `cache_read`) | tokens reported in usage records |
| `parley_audit_records` | gauge | `kind` (`hub_event`, `run_event`, `outcome`, `tool_call`, `tool_error`, `trace`, `postmortem`, `session`, `usage`) | stored records, sampled by retention |
| `parley_jobqueue_depth` | gauge | `type` (`rebuild`), `status` (`queued`, `running`, `failed`, `completed`, `dead_letter`, `cancelled`) | durable jobs by type and status |
| `parley_retention_errors_total` | counter | `step` (the eleven step names) | failed retention steps |
| `parley_retention_last_run_timestamp_seconds` | gauge | none | Unix time of the last fully successful retention run |
| `parley_sandboxes` | gauge | `state` (`provisioning`, `ready`, `stopped`, `completed`, `failed`) | sandboxes by state |
| `parley_sandbox_creates_total` | counter | `result` (`ready`, `failed`) | sandbox creations by outcome |
| `parley_sandbox_seconds` | histogram | none | time from `ready` to stop, exit or deletion. Buckets 60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800, 86400 |
| `parley_sandbox_exec_total` | counter | `result` (`ok`, `nonzero`, `timeout`, `error`) | commands run through exec |
| `parley_sandbox_denials_total` | counter | none | connections and requests the sandbox runtime blocked |
| `parley_sandbox_checkpoints_total` | counter | `result` (`ok`, `empty`, `failed`) | checkpoints attempted |
| `parley_sandbox_runtime_up` | gauge | none | 1 while the sandbox runtime is reachable |

The sandbox metrics are exported with value 0 when sandboxes are disabled.

Other packages change these values in two ways. They **report** an increment, a value or an observation to the recorder, or they **register a source** for a counter or gauge family, a function the exposition calls at every scrape (the job queue's counts, the number of open changes). This package itself counts every HTTP request, with the route template as `path`, never the concrete URL, and measures its duration.

No package built before this one imports `internal/metrics`. Each either exports a plain value (a counter, a count, an observer registration), which this package connects in `internal/hub`, or reports to a recorder interface it declares itself, which this package fills with the registry in `internal/hub`. Package 53 is built after this one and reports by itself. Every metric of the catalogue has exactly one reporter, the package that decides when its value changes, and every connection uses the recorder's method names:

| Metric | Reporter and what it exports | Connection |
|---|---|---|
| `parley_http_requests_total`, `parley_http_request_duration_seconds` | this package: the HTTP instrumentation | `Add`, `Observe` |
| `parley_git_requests_total` | package 17: the request counter of `internal/gitserver` (package 18 never increments it) | `RegisterSource` reading that counter |
| `parley_scm_requests_total` | package 20: the request recorder interface of `internal/scm` (`op`, `result`), through which package 21 reports its writes as well | this package fills it with an adapter that calls `Add` |
| `parley_changes_open` | package 22: the change store | `RegisterSource` counting the changes in state `draft` or `open` per workspace through the exported read `change.OpenCounts(ctx)` |
| `parley_lands_total`, `parley_land_attempts`, `parley_land_duration_seconds` | package 25: the final observer registration of `internal/land` | this package registers one observer: `Add` with the final status as `result`, `Observe` with `attempts`, `Observe` with `finished_at` minus `created_at` in seconds |
| `parley_forks_active` | package 16: the workspace records of kind task fork | `RegisterSource` counting the task forks that exist through the exported read `fork.CountTaskForks(ctx)` |
| `parley_jobqueue_depth` | package 33: `Depth(ctx)` of `internal/jobs` | `RegisterSource` |
| `parley_sandbox_runtime_up` | package 44, on the recorder it is given | `Set` |
| `parley_sandboxes`, `parley_sandbox_creates_total`, `parley_sandbox_seconds` | package 45, on the recorder it is given | `Set`, `Add`, `Observe` |
| `parley_sandbox_exec_total` | package 46, on the recorder it is given, one `Add` with `result` when an exec ends | `Add` |
| `parley_sandbox_checkpoints_total` | package 47, on the recorder it is given | `Add` |
| `parley_sandbox_denials_total` | package 48, on the recorder it is given | `Add` |
| `parley_agent_tokens_total` | package 49: `session.OnUsage(observer)` | this package registers an observer that calls `Add` once per direction with a count above 0 |
| `parley_agent_sessions_active` | package 49: `session.ActiveCounts(ctx)` | the `gauges` step calls `SetFamily` with the result |
| `parley_audit_events_total` | package 50: `telemetry.OnRunEvent(observer)`; package 52: `Counters().written_by_type` | one source registered with `RegisterSource`: the `agent` series from a tally the run event observer fills, the `hub` series from `written_by_type` |
| `parley_events_seq` | package 52: `Counters().last_seq` | `RegisterSource` |
| `parley_sse_connections` | package 52: `Counters().open_streams` | `RegisterSource` |
| `parley_webhook_deliveries_total` | package 53 | package 53 calls `Add` with `result` on the recorder it is given |
| `parley_webhook_queue_depth` | package 53: its queue-depth function | package 53 calls `RegisterSource` |
| `parley_audit_records`, `parley_retention_errors_total`, `parley_retention_last_run_timestamp_seconds` | this package | `Set`, `Add`, `Set` |

**The sandbox packages' recorder.** Package 44 declares in `internal/sandbox/metrics.go` the recorder interface that packages 44 to 48 report through: `Add`, `Set` and `Observe`, each with a metric name and a label map, with a default that records nothing. That is the method set of this package's recorder without `SetFamily`, with the same signatures, so the registry satisfies it as it is. `internal/hub` passes the registry to the sandbox packages as that recorder, in place of the default, before their components are built. This package computes none of the seven sandbox metrics: it does not derive `parley_sandbox_exec_total` from the sandbox exec records of package 50 or from the operation observers of package 46, which reports the metric itself. While sandboxes are disabled nothing is reported and the families stay at 0.

### End-to-end flows

1. **An operator audits the hub.** An agent ingests a run event for a run of workspace `demo`; a developer's action on `demo` writes a hub event. The operator runs `parley audit query --workspace demo` with the admin token. `parleyd` authenticates, merges the hub events and run events of `demo`, newest first, and answers the list. `parley CLI` prints it; both records are in it, each with its `source`.
2. **A developer inspects a run.** An agent ingests traces for one node of a run. The developer reads `GET …/runs/:run_id/traces` and then `GET …/runs/:run_id/transcript?node_id=…`. `parleyd` answers the traces in ascending order and the conversation with roles.
3. **Retention keeps the record bounded and the operator watches it.** The operator starts `parleyd` with `[audit].max_age = "1d"` on data that holds run events older than a day. `parleyd` runs the retention steps in order at start, deletes the old records and sets the last-run timestamp. The operator reads `GET /metrics` without a credential and sees every metric of the catalogue, the timestamp and the recomputed record counts.

## Design Decisions

1. **Project decisions relied on.** Go; embedded SQLite through `modernc.org/sqlite` behind `internal/store`, the analytical store being a second file; `net/http` with its method-and-pattern router; the CLI on `github.com/spf13/cobra` through the command helper of package 09; table-driven tests with `testing` and `net/http/httptest`; a replaceable clock.
2. **No metrics library.** The registry and the text exposition are written in `internal/metrics` on the standard library. Why: the catalogue is fixed and small, the format is a few lines per family, and the rule "every closed label value present at 0 from start" is simpler to guarantee in one table than through a client library.
3. **One recorder, three calls, and sources.** The recorder has `Add` (counter), `Set` (gauge) and `Observe` (histogram), each taking the metric name, a label map and the value and returning an error, plus `SetFamily`, which replaces every series of a gauge family. A package whose value lives in its own store or its own counter registers a source for that family instead; the exposition calls it at every scrape. Histograms take observations only. Why: packages already written describe both styles (20 reports to a recorder, 17 keeps its own counter, 33 supplies counts).
4. **A wrong report is refused, not fatal.** A report or a source registration that names a metric outside the catalogue, uses other label names than the catalogue's, uses a value outside a closed set, uses the wrong call for the metric's type or adds a negative amount to a counter returns an error and records nothing. A source that fails or panics exports its family at zero for that scrape and is logged. A scrape never fails because of a reporter.
5. **Families with an open label.** While a family whose labels are not all closed has no series (`workspace`, `model`, `type`, `method`, `path`, `status`), it is exported with one zero-valued series per combination of its closed label values, with each open label set to the empty string. The placeholder is no longer exported once the family has a series. Why: AUD-FR-061 demands at least one series from start-up, and a real workspace or path does not exist yet.
6. **The `path` label.** It is the path of the router pattern that matched (for example `/api/v1/workspaces/{slug}/sync`), without the method. A request that matched no pattern is counted with `path` `unmatched`. `status` is the three-digit code that was sent; `method` is the request's method in upper case. `/metrics` itself is counted. Why: a concrete URL as a label value would grow without bound.
7. **Exposition order.** Families appear in the order of the catalogue, each with one `# HELP` and one `# TYPE` line; series are sorted by their label values. Label values are escaped as the text format requires (backslash, double quote, line feed). Why: a stable output can be compared in tests and diffed by operators.
8. **Where the read code lives.** The per-run read routes are in `internal/telemetry`, beside the store of package 50; the audit query and retention are in `internal/audit`. The store operations retention needs (delete by age, time out, trim) are added beside each store in a file named `retention.go`. Why: the brief's layout puts telemetry reads in `internal/telemetry`, and a store's SQL belongs with the store.
9. **`since` before `until` on per-run lists too.** AUD-FR-032 states the rule for the audit query only; the per-run lists use the same validator, so `since` not before `until` is 400 there as well.
10. **Enumerated filters.** `severity`, `order`, `source` and `actor_type` are validated against their sets. On `…/traces` the `event_type` filter is validated against the five trace types, because a trace's `event_type` is an enumeration; on `…/events` it is free-form. A malformed `run_id` filter on `GET /audit` is 400, like the one in a path.
11. **Unknown query parameter names are ignored**, as unknown body members are (PLAT-FR-017). AUD-FR-034 is read as a rule about the values of known parameters.
12. **Audit order is total.** After time and `id`, a hub item sorts before an agent item, so the order is total even if the two sources held the same id.
13. **The `workspace` filter and gone workspaces.** The filter is accepted when the caller is an admin token, when the caller can read the workspace, or when the slug is a task fork's (`<parent>--<name>`) and the caller can read the parent, which covers a fork that was deleted. Every other case is 404. Run events of a workspace that no longer exists are returned to admin tokens, and those of a deleted fork also to readers of its parent, matching §14.10 "Deleted workspaces".
14. **The audit query returns what is stored.** It applies no age limit of its own; `[audit].max_age` takes effect through the `hub_events` and `run_records` steps.
15. **Transcript order is total.** Traces are ordered by `timestamp`, then `sequence` with an absent `sequence` counting as 0, then `id`. The cursor counts skipped `session.result` records as consumed.
16. **"Older than" is strict.** A record whose time equals the cutoff (now minus the limit) is kept.
17. **A timed-out session gets a duration.** `stale_sessions` sets `duration_ms` from `started_at` and `ended_at`, as §14.3.2 says the hub does when a client sends none. It writes no hub event: the catalogue of §14.8 has none for a timeout.
18. **What counts as a run for `max_runs`.** A run counts when the workspace holds at least one run event, outcome, tool call, tool error or trace for it. A run with only a postmortem does not count. Among runs with the same earliest record, the lower `run_id` is the older.
19. **`hub_events` keeps the newest event** even when it is older than `[audit].max_age`, because §14.7.2 says the newest event always stays in the event log.
20. **Orphans.** A slug is orphaned when no workspace has it and, for a fork slug, when the parent named before `--` does not exist either. A record's age is the time its own step uses: `started_at` of a session, `reported_at` of a usage record, `timestamp` of telemetry, `ingested_at` of a postmortem, `time` of a hub event. Hub events without a workspace are never orphans.
21. **`webhook_deliveries` is a slot.** Package 53 is not a dependency of this one, so the step calls the interface `DeliveryPruner` declared in `internal/audit`, with one operation `PruneDeliveries(ctx, before)` that deletes the delivery records that are not `pending` and whose last attempt is before the given time and returns the number deleted. Package 53, which depends on this package, registers its delivery store through `audit.RegisterDeliveryPruner` from `internal/hub`. While no pruner is registered the step succeeds and deletes nothing.
22. **Each step is its own unit of work.** A step runs in its own transaction or transactions; a failure rolls back that step only. A step that panics counts as failed. When the run's context is cancelled (shutdown), the run stops before the next step, counts no error for the steps it did not start and does not set the timestamp.
23. **Key ownership.** This package registers the five retention keys of `[audit]` and both keys of `[sessions]`. `[audit].write_timeout` is package 50's; the `[events]` keys are package 52's, and this package reads the loaded `[events].retention` through package 52's `Retention()` for the `event_log` step. The cross-key rule `[events].retention` ≤ `[audit].max_age` is checked here, when the retention settings are built at start (package 52's Design Decision 19 leaves it to this package). Each key is registered exactly once across packages 49 to 53: none by 49, one by 50, seven here, two by 52 and the four `[webhooks]` keys by 53.
24. **The CLI validates nothing the hub validates.** `parley audit query` passes flag values through; only a `--limit` that is not an integer and a positional argument are usage errors.
25. **Stores of other packages.** The four things retention prunes are reached like this. Sessions (package 49): this package adds `TimeOutActiveBefore` and `DeleteTerminalBefore` beside its store in `internal/session/retention.go`, and reads `session.ActiveCounts` for the gauge. Telemetry (package 50): this package adds the deletes by age, by run count and by orphaned slug in `internal/telemetry/retention.go`. The event log (package 52): the `event_log` step calls that package's `Expire(cutoff)` and adds nothing; the deletes of stored hub events for `hub_events` and `orphans` are added by this package in `internal/event/retention.go`. Webhook deliveries (package 53): `DeliveryPruner`, decision 21.
26. **Connecting the producers' values** (the table of the Metrics section). Packages 49, 50 and 52 are dependencies of this one and cannot import its registry's wiring, so they export observers and counters and this package connects them in `internal/hub` with `Add`, `SetFamily` and `RegisterSource`. `parley_audit_events_total` has two producers and a metric takes one source, so this package registers one source that merges both. The same holds for the feature packages built earlier that leave their metric to this one (17, 20, 22, 25, 16, 33): their counter, request recorder, store read, final observer and `Depth` are connected in `internal/hub`, where every earlier package can be imported.
27. **The sandbox metrics go through the interface of package 44.** Package 44 declares the recorder interface of the sandbox packages in `internal/sandbox/metrics.go` (`Add`, `Set`, `Observe` with a metric name and a label map) as the method set of this package's recorder, and no sandbox package depends on this one. The registry satisfies that interface without an adapter and `internal/hub` passes it to packages 44 to 48. Each sandbox metric is reported by the package whose behaviour it counts: `parley_sandbox_runtime_up` by 44; `parley_sandboxes`, `parley_sandbox_creates_total` and `parley_sandbox_seconds` by 45; `parley_sandbox_exec_total` by 46; `parley_sandbox_checkpoints_total` by 47; `parley_sandbox_denials_total` by 48. This package computes none of them. A report the registry refuses (decision 4) returns its error to the reporting package and records nothing.
28. **Profile: full.**
29. **The label values of `parley_scm_requests_total` are the union of what packages 20 and 21 report.** §14.7.3 lists the `op` values `refs`, `commits`, `tree`, `file`, `diff`, `compare`, `blame`, `search`, `commit` and `rebase`. Package 20 reports the eight read values with `result` `ok` or `error`; package 21 reports `commit`, `ref_create`, `ref_move`, `ref_delete` and `rebase` with `result` `ok`, `error`, `ref_moved` or `refused`. The catalogue therefore has thirteen `op` values, three more than the PRD (`ref_create`, `ref_move`, `ref_delete`, the ref routes of package 21), and the PRD's four `result` values, 52 series from start-up. Why: the registry refuses a report outside the catalogue (decision 4), so a value package 21 reports and the catalogue lacks would never be counted.

## Dependencies

- Package 50 `telemetry_ingestion` (draft): the telemetry record store in `internal/telemetry` that the per-run reads, the audit query and the retention steps read and prune. Through it, package 49 `agent_sessions_and_usage`: the session store the session steps act on.
- Package 52 `durable_event_log_and_stream` (draft): the hub event store and the visibility rule in `internal/event`, read by the audit query and trimmed by the `event_log` and `hub_events` steps.
- Package 09 `cli_foundation` (draft): the HTTP client, output, exit codes and command helper that `parley audit query` is built on (added to the plan's list as the brief requires for a CLI command).
- Reached through those packages, not plan dependencies: 02 (error envelope, list helper, router), 03 (configuration key registry, maintenance task registry, hub composition), 06 (scope check), 07 (access component, readable set), 08 (binding of a bound token).
- Package 53 `event_webhooks` depends on this package: it registers its delivery store as the `DeliveryPruner` (decision 21) and reports its two metrics to the recorder. Packages 44 to 48 report the sandbox metrics through the recorder interface package 44 declares in `internal/sandbox`, which `internal/hub` fills with this package's registry; the feature metrics of packages 16, 17, 20, 22, 25 and 33 are connected by this package in `internal/hub` (the table of the Metrics section).

## Verified External API

`github.com/spf13/cobra` v1.10.2, read in the module cache (`command.go`, `args.go`):

- `func (c *Command) AddCommand(cmds ...*Command)`
- `func (c *Command) Flags() *pflag.FlagSet`
- `func NoArgs(cmd *Command, args []string) error`

`github.com/spf13/pflag` v1.0.10, read in the module cache (`string.go`, `int.go`, `flag.go`):

- `func (f *FlagSet) StringVar(p *string, name string, value string, usage string)`
- `func (f *FlagSet) IntVar(p *int, name string, value int, usage string)`
- `func (f *FlagSet) Changed(name string) bool`

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Agent telemetry**: Run events, tool calls, traces, outcomes and postmortems that agents send to the hub.
- **Run id**: The identifier of one agent run, `YYYYMMDD_HHMMSS_xxxxxx` with six lower-case hexadecimal digits.
- **Event log**: The part of the hub events addressable by `seq`: events newer than `[events].retention`.
- **Audit log**: Every hub event newer than `[audit].max_age`.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Readable set**: The workspaces a caller may see in cross-workspace reads.
- **Closed label**: A metric label whose values the catalogue lists in full.
- **Source (metrics)**: A function a package registers for a counter or gauge family; the exposition calls it at every scrape.
