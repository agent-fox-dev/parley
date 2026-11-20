# Performance targets

The hub's performance targets are an initial bar, to be revisited with
measurements. `test/perf` measures them against one running hub: it is a
black-box HTTP client of that hub and imports no code of the project. A
missed target never fails a test run; the report says, per target, what was
measured and whether the target was met.

The targets are stated for a host of 8 cores and 32 GB with repositories of
up to 5 GB, so the harness runs against a hub the operator provides, never
in the ordinary test run.

## Running it

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

With `PARLEY_PERF_URL` set, the test fails only when the harness cannot do
its job: `PARLEY_PERF_TOKEN` or `PARLEY_PERF_WORKSPACE` is unset (the
message names the variable), `GET /version` does not answer 200 within 10
seconds (the message names the URL, and no scenario runs), or the report
cannot be written (the message names the path). A missed target, a skipped
scenario and a scenario that ends in an error never fail it.

## Scenarios

Fourteen, run one after the other in this order:

| Name | What is done | Measured | Target |
|---|---|---|---|
| `fork_create` | 50 forks created one after the other | p95 of the request duration, ms | under 1000 |
| `file_read` | 200 reads, alternately a file and a tree | p95, ms | under 100 |
| `commit_create` | 50 commits of 100 files each on a branch of the run | p95, ms | under 500 |
| `land` | 20 changes of 20 commits each with disjoint files, landed one at a time | p95 of the time from the land request to the land attempt's success, ms | under 2000 |
| `land_throughput` | 120 one-commit changes with disjoint files, at most 8 land requests in flight | changes landed × 60 ÷ seconds elapsed | at least 60 per minute |
| `land_concurrency` | 16 land requests for 16 non-conflicting changes sent at the same time | land attempts that did not end in success | at most 0 |
| `mergeability` | 100 open changes; the target branch moved by landing one more | seconds until none of the 100 reports `mergeable` `unknown` (polled at most once a second) | at most 60 |
| `event_latency` | a subscriber on `GET /events`; 50 commits created one after the other | the largest time from a commit's response to its event's arrival, ms | at most 1000 |
| `active_forks` (capacity) | forks created over all listed workspaces until 5,000 exist or every workspace refuses | forks that exist at once | at least 5000 |
| `open_changes` (capacity) | changes opened in the first workspace until 2,000 are open or the hub refuses | open changes | at least 2000 |
| `event_streams` (capacity) | `GET /events` connections opened until 500 are open or the hub refuses | connections open at once | at least 500 |
| `sandboxes_running` (capacity, sandbox) | sandboxes created until 200 are `provisioning` or `ready` or the hub refuses | sandboxes in those states | at least 200 |
| `sandbox_ready` (sandbox) | 5 sandboxes created one after the other | the largest time from the create response to state `ready`, s | at most 60 |
| `sandbox_exec` (sandbox) | 50 execs of the command `true` in one sandbox | p95 of the request duration, ms (an upper bound of the hub's overhead) | under 200 |

p95 is the nearest-rank percentile: of n samples sorted ascending, the one
at position ⌈0.95·n⌉.

A scenario is `met` when its measurement satisfies the target, `missed` when
it does not, `skipped` when the variable it needs is not set (the detail
names the variable), and `error` when a request that is not its expected
stop is answered with 400 or more or fails in transport, or when it runs
past its deadline (10 minutes; 60 for a capacity scenario). The detail of an
`error` names the method, the path, the status and the `error_type` of the
first failing request, or says `deadline exceeded`. For a capacity scenario
the hub's refusal of a creation is the expected stop: the count reached is
the measurement, and the detail says `refused with <status> <error_type>
after <count>`, then the method, the path and the hub's message;
`active_forks`, which fills each workspace in turn, writes one
`<workspace> refused with <status> <error_type> after <count>` per
workspace that refused, separated by `; `. A scenario that ends in `error` does not stop the run.

The default limits (200 forks per workspace, 20 sandboxes per user) are
below two capacity targets: list enough workspaces or raise the limits, and
the report shows what was reached.

## What it leaves behind

The harness never writes to a workspace's default branch. It creates a
target branch `perf/<run id>` at the head of the default branch of the
first workspace, source branches `perf-<run id>/<scenario>/<n>`, and forks
of its own. When a scenario ends, in any verdict, it closes the event
streams it opened, deletes the forks and sandboxes it created and abandons
the changes `open_changes` opened. A clean-up request that fails is listed
in the report's `cleanup_errors` and changes no verdict. The branches, and
the changes the latency scenarios landed or left open, stay.

## The report

One JSON document, written to the report path and logged by the test as a
table:

```json
{"hub":{"url":"https://hub.example","version":{"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-01-02T03:04:05Z"}},
 "run_id":"9c1e0f3a7b2d","started_at":"2026-11-20T09:00:00Z","finished_at":"2026-11-20T09:41:12Z",
 "results":[{"name":"fork_create","target":{"comparison":"lt","value":1000,"unit":"ms"},
   "measured":412.5,"samples":50,"status":"met","detail":""}],
 "cleanup_errors":[]}
```

`results` holds the fourteen scenarios in the order above, whatever
happened. `comparison` is `lt`, `le` or `ge`; `measured` is `null` when
there is no measurement; `hub.version` is the document `GET /version`
returned. The token appears nowhere in the report, and text taken from a
hub response is cut to 200 bytes.
