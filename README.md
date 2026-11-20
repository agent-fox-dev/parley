# parley

Where agents negotiate with your code - Rarr !

Parley is the server that AI coding agents, and the people who run them, work
through. It holds everything an agent run needs except the agent itself: the
repository, the credentials, the secrets, a sandbox to run in, and a record of
what happened and what it cost.

## Why

Pointing coding agents at real repositories raises the same questions every
time:

- **Where does the agent's copy of the code live**, and how do several agents
  work on one repository without stepping on each other?
- **How do their changes get back** into the main branch without a person
  rebasing and merging every branch by hand?
- **Which credentials does an agent get?** Forge tokens and API keys copied
  into an agent's environment are hard to scope and harder to revoke.
- **Where does the agent run**, isolated from the machine it was started on?
- **What did it do, and what did it cost?**

Without a common answer, every team scripts its own: tokens end up in
environment files, branches are merged by whoever notices, and the trail of
what an agent did is scattered across terminals.


## Run it

Parley is two programs: `parleyd`, the hub, and `parley`, its client. Build
them with Go 1.27 (`go build ./cmd/...`). The hub needs `git` 2.38 or later
on its `PATH` and refuses to start without it; it keeps its data in two
embedded SQLite stores and a directory of git repositories.

- `parleyd --admin-email you@example.com` bootstraps a new hub and writes an
  admin token file; `parleyd` then serves on `0.0.0.0:8080` (see
  [CLI](docs/cli.md#parleyd)).
- `parleyd backup <directory>` writes a snapshot while the hub runs;
  restoring is copying the snapshot into place.
- `containers/hub/Dockerfile` builds the hub image (user 10001, its config
  and data on the volume `/var/lib/parley`, port 8080),
  `containers/sandbox/Dockerfile` a reference sandbox image, and
  `deploy/kubernetes/` runs one hub on a persistent volume (see
  [Architecture](docs/architecture.md#images-and-manifests)).
- `go test ./test/perf/` with `PARLEY_PERF_URL` set measures the
  performance targets against a running hub (see
  [Performance](docs/performance.md)).

## Read more

- [API reference](docs/api.md) — every route, what it answers and how it fails
- [Configuration](docs/configuration.md) — the configuration file and everything read from the environment
- [Architecture](docs/architecture.md) — how the service is put together
- [CLI](docs/cli.md) — Parley's client