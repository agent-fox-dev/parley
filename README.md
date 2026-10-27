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

## Install

Parley runs on Node 22 or newer.

```sh
git clone https://github.com/agent-fox-dev/parley.git && cd parley
npm install
```

## Develop

```sh
make help     # list every target
make check    # the fast gate: formatting, static checks, lint and the tests
make test     # the tests alone
```

`make check` is the gate to run before committing. `make check-release`
adds the slow suites and is run by a person, before a release or a merge.

## Run it without Cloudflare

Parley runs on one Linux or macOS host with no Cloudflare account and no
network access to Cloudflare, using SQLite on the host's own disk instead of
D1 and the process environment instead of Worker bindings.

```sh
make build                      # type-check and bundle to dist/parley.mjs
PARLEY_PORT=8787 node dist/parley.mjs
curl localhost:8787/healthz     # {"status":"ok"}
```

See [configuration](docs/configuration.md) for what it reads from the
environment, and [the architecture](docs/architecture.md#two-platform-targets)
for why the two targets exist.

## Read more

- [API reference](docs/api.md) — every route, what it answers and how it fails
- [Configuration](docs/configuration.md) — everything read from the environment
- [Architecture](docs/architecture.md) — how the service is put together
- [CLI](docs/cli.md) — Parley's client is `afc`, from `af-hub`
