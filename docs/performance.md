# Performance targets

## Why there are targets

Parley is built for many agents working on one repository at once. That
only works if the things an agent does all day are fast enough not to be
worth avoiding: forking for a task, reading a file, committing, landing.
The targets put a number on each of them, and on how much one hub holds,
so that "fast enough" can be measured and a regression noticed.

They promise this much: one hub on a host with 8 cores, 32 GB of memory and
local SSD storage, with repositories of up to 5 GB of history, meets them.
They are an initial bar, to be revisited with measurements, not a
guarantee for every host. Because they are stated for a host of that size,
they are measured against a hub you provide and never in the ordinary test
run, and a missed target never fails a test: the report says what was
measured and whether the target was met.

## The targets

Fourteen scenarios, run one after the other in this order:

| Scenario | What is done | Measured | Target |
|---|---|---|---|
| `fork_create` | 50 forks created one after the other | p95 of the request duration | under 1000 ms |
| `file_read` | 200 reads, alternately a file and a tree | p95 | under 100 ms |
| `commit_create` | 50 commits of 100 files each | p95 | under 500 ms |
| `land` | 20 changes of 20 commits each, landed one at a time | p95 from the land request to the attempt's success | under 2000 ms |
| `land_throughput` | 120 one-commit changes, at most 8 land requests in flight | changes landed per minute | at least 60 |
| `land_concurrency` | 16 non-conflicting changes landed at the same time | land attempts that did not succeed | 0 |
| `mergeability` | 100 open changes, then the target branch moves | seconds until none reports `mergeable` `unknown` | at most 60 |
| `event_latency` | 50 commits with a subscriber on the event stream | the largest time from a commit's response to its event | at most 1000 ms |
| `active_forks` (capacity) | forks created over all listed workspaces | forks that exist at once | at least 5000 |
| `open_changes` (capacity) | changes opened in the first workspace | open changes | at least 2000 |
| `event_streams` (capacity) | event streams opened | streams open at once | at least 500 |
| `sandboxes_running` (capacity, sandbox) | sandboxes created | sandboxes `provisioning` or `ready` | at least 200 |
| `sandbox_ready` (sandbox) | 5 sandboxes created one after the other | the largest time from the create response to `ready` | at most 60 s |
| `sandbox_exec` (sandbox) | 50 execs of `true` in one sandbox | p95 of the request duration, an upper bound of what the hub adds | under 200 ms |

p95 is the nearest-rank percentile: of n samples sorted ascending, the one
at position ⌈0.95·n⌉. A capacity scenario creates until it reaches its
target or the hub refuses; the count reached is the measurement.

## Measuring a hub

The harness in `test/perf` is an HTTP client like any other: it measures
what a client sees. Give it a running hub, a user's credential with write
access, and one or more cloned workspaces:

```sh
PARLEY_PERF_URL=https://hub.example PARLEY_PERF_TOKEN=... PARLEY_PERF_WORKSPACE=acme-app \
  go test ./test/perf/ -run TestPerformanceTargets -count=1 -timeout 3h -v
```

| Variable | Meaning |
|---|---|
| `PARLEY_PERF_URL` | the hub's base URL; unset, the test is skipped |
| `PARLEY_PERF_TOKEN` | a user's credential with write access to the workspaces; required |
| `PARLEY_PERF_WORKSPACE` | slugs of cloned workspaces, separated by commas; required. Every scenario uses the first; `active_forks` uses all |
| `PARLEY_PERF_SANDBOX_PROFILE` | a sandbox profile; unset, the three sandbox scenarios are `skipped` |
| `PARLEY_PERF_CAPACITY` | `1` runs the four capacity scenarios; otherwise they are `skipped` |
| `PARLEY_PERF_REPORT` | where the report is written; default `parley-perf-report.json` in the system's temporary directory |

The test fails only when the harness cannot do its job: a required
variable is unset, the hub does not answer `GET /version` with 200 within
10 seconds, or the report cannot be written.

The harness never writes to a default branch. It works on a branch
`perf/<run id>` and branches `perf-<run id>/<scenario>/<n>` of the first
workspace and on forks of its own. After each scenario it deletes its forks
and sandboxes, closes its streams and abandons the changes `open_changes`
opened. The branches, and the changes the latency scenarios landed or left
open, stay: use a workspace made for measuring.

## Reading the report

The test logs a table and writes one JSON document:

```json
{"hub":{"url":"https://hub.example","version":{"version":"1.4.0","commit":"3f2a1bc","build_time":"2026-01-02T03:04:05Z"}},
 "run_id":"9c1e0f3a7b2d","started_at":"2026-11-20T09:00:00Z","finished_at":"2026-11-20T09:41:12Z",
 "results":[{"name":"fork_create","target":{"comparison":"lt","value":1000,"unit":"ms"},
   "measured":412.5,"samples":50,"status":"met","detail":""}],
 "cleanup_errors":[]}
```

`results` always holds the fourteen scenarios in order. `comparison` is
`lt`, `le` or `ge`; `measured` is `null` when there is no measurement;
`hub.version` is what `GET /version` returned. The token never appears, and
text from a hub response is cut to 200 bytes.

| `status` | Meaning |
|---|---|
| `met` | the measurement satisfies the target |
| `missed` | it does not |
| `skipped` | a variable the scenario needs is not set; `detail` names it |
| `error` | a request was answered with 400 or more or failed in transport, or the scenario ran past its deadline (10 minutes, 60 for a capacity scenario); `detail` names the first failing request or says `deadline exceeded` |

For a capacity scenario, `detail` says where the hub refused: `refused
with <status> <error_type> after <count>`, per workspace for
`active_forks`. `cleanup_errors` lists clean-up requests that failed; they
change no verdict. An `error` in one scenario does not stop the others.

## When a target is missed

- **Check the host first.** The targets hold for 8 cores, 32 GB and local
  SSD storage. On a smaller or busier host a miss says little.
- **Check that the hub's limits allow the target.** The defaults of
  [configuration.md](configuration.md#server-and-storage) are below two
  capacity targets: 200 forks per workspace (list at least 25 workspaces
  or raise `[forks].max_per_workspace`) and, for
  [sandboxes](configuration.md#sandboxes), 20 per user and 50 per
  workspace against a target of 200. A capacity `detail` that names a
  limit is a configuration finding, not a performance one. A scenario
  that ends in `error` with a `429` met the rate limit: raise
  `[limits].requests_per_minute` for the run.
- **Then treat it as a regression.** Run again to rule out noise, compare
  with the report of the previous release (`hub.version` tells them
  apart), and report the scenario, both measurements and the host.
