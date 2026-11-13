# Steering Directives

Project-level directives for every agent and skill working on this repository.
The shared directives come first; anything that is specific to this repository
lives in the **Repository Specifics** section at the end.

## Library Reuse

1. Before implementing any functionality, check whether it already exists in the codebase.
2. **Declared dependencies** — check the project's dependency manifest and existing imports before adding a new dependency.
3. **Standard library** — prefer the language's standard library over third-party alternatives.

## Target Platforms

The software MUST run on Linux or macOS systems.
Support for Windows or other operating systems is "best effort", but not a requirement.

## Documentation Freshness

After implementing any spec, you **must** update all affected documentation
before the session is considered complete. Outdated docs are treated as
regressions.

Before updating the documentation, run `make doc` first, to refresh the
generated API docs (see below).

**When to update:** Every time a spec implementation adds, changes, or removes
any of the items listed below, update the corresponding document in the same
session — not as a follow-up task.

| What changed | Update |
|---|---|
| REST endpoints or the public API added/changed/removed | `docs/api.md` |
| CLI commands, subcommands, or flags added/changed/removed | `docs/cli.md` |
| Config keys or env vars added/changed | `docs/configuration.md` |
| Architecture, package layout, or data flow changed | `docs/architecture.md` and/or relevant ADR |
| Setup, quickstart, or project overview changed | `README.md` |

Do not write tests that check whether a doc or PRD exists or what it says.

Write the actual content, never "TODO" or "TBD". A doc listed above that does
not exist yet is created, not skipped; a repository without a REST API or CLI
keeps the file anyway and says in one line that it has none. Run `make check`
after the doc updates.

## Tests and Spec Traceability

Specs are the input to an implementation, not the shape of its tests or its
comments. Once a spec is implemented, the code, its tests and the ADRs are
the record; the spec is archived.

- **Tests are organised by behaviour, not by spec.** A test file is named for
  the area of the production code it tests (e.g. `create`, `sync`, `access`),
  and a test for the behaviour it checks (e.g.
  `Create rejects a duplicate slug`). Do not create per-spec test files,
  smoke, property, phase or group files, or test names and comments that
  carry spec or test-case ids (`TS_01_17`, `01-REQ-4.1`). The variants of one
  behaviour are one table-driven (parameterised) test.
- **Every behaviour is tested once**, at the level where it lives: a handler
  through the router, a store function directly, a pure function directly.
  Keep end-to-end tests (a real server, a built binary, real credentials) for
  what only that layer proves. The test code of a module stays near the size
  of its production code.
- **Use the shared helpers** (named under Repository Specifics) for fixtures,
  credentials, requests, responses and seeds; a module keeps one helpers file
  for what is specific to it. Create another module's tables with that
  module's schema function, never by copying its DDL.
- **Do not test artefacts**: no tests of an import graph, a build tag, the
  presence of a helper, a struct's fields, a documentation file's content, or
  that a module boundary holds.
- **Production comments give the reason, not a citation.** No spec or
  requirement ids in code comments; where a decision is the reason, cite the
  ADR.
- **Traceability lives with the work.** The map from a spec's test ids to the
  tests that cover them is written in the commit that implements the task,
  not in the code.

## Generated Docs

`make doc` writes the API reference of every publicly accessible module of the
repository (no internal modules, no examples) to `docs/generated/`, one text
file per module, using the language's own doc tool. The files are committed to
git.

Their purpose is orientation: **when you start working on this repository, use
`docs/generated/` to find your way around.** It is the cheapest way to see which
modules exist, what each one is for and which types, functions and constants
it exports, before reading any source. Prefer it to scanning directories or
grepping for declarations.

- Never edit these files by hand; they are overwritten by `make doc`.
- Treat them as possibly stale: the code is authoritative. If a file disagrees
  with the source, regenerate it.
- Run `make doc` and commit the result whenever a change adds, removes or
  alters an exported identifier or a module's doc comment.

## Repository Specifics

- Go module `github.com/agent-fox-dev/parley` (`go 1.27`); lint configuration
  is `.golangci.yml`.
- Spec packages live in `.specs/`; the product PRD is split per section in
  `docs/prd/`.
- Shared test helpers live in `internal/hubtest`, imported by test code
  only: `hubtest.Start` bootstraps a hub with an admin token and serves it
  in-process on a free loopback port (its `Hub` has `Token`, `Store`,
  `URL(path)`, `CreateUser` and `IssueKey`), `hubtest.Serve` runs a hub
  with the options a test gives until the test ends, and `hubtest.LogBuffer`
  is the buffer a running hub may write its log to while the test reads it.
  A test that needs a running hub starts it with these.