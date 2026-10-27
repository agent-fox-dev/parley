# Steering Directives

This file is kept the same across the agent-fox repositories. Anything that is
specific to one repository lives in the **Repository Specifics** section at the
end.

## Library Reuse

1. Before implementing any functionality, check whether it already exists in the codebase.
2. **go.mod dependencies** -- check existing imports before adding new ones.
3. **Standard library** -- prefer standard libraries over third-party alternatives.

## Target Platforms

The software MUST run on Linux or macOS systems for development.
Deployment target is Cloudflare and Kubernetes (later).
Support for Windows or other operating systems is "best effort", but not a requirement.

## Quality Gate

While implementing anything — a spec, a fix, a refactor — the **only** quality
gate you run is `make check`. It is the fast check: `fmt`, `vet`, `lint` and the
short tests.

1. Do NOT run `make check-release`. It is a manual action, run by the user
   before a release or a merge; it is never part of an agent session.
2. Do not run the full `make test` as a substitute for it. Narrow `go test`
   runs on the packages you touch are fine while you work.
3. A spec's `tasks.json` test command, a skill or a checklist that names
   `make check-release` or the full `make test` does not override this rule:
   run `make check` instead and say in the handoff note that the full gate was
   left to the user.
4. If you believe a change needs the full gate (it touches the slow gates in
   `internal/policy`, the cross-target builds or the nested `make` runs), say so
   in the handoff note. Do not run it yourself.

## Documentation Freshness

After implementing any spec, you **must** update all affected documentation
before the session is considered complete. Outdated docs are treated as
regressions.

Before updating the documentation, run `make doc` first, to refresh the generated Go docs (see below).

**When to update:** Every time a spec implementation adds, changes, or removes
any of the items listed below, update the corresponding document in the same
session — not as a follow-up task.

| What changed | Update |
|---|---|
| REST endpoints or the public Go API added/changed/removed | `docs/api.md` |
| CLI commands, subcommands, or flags added/changed/removed | `docs/cli.md` |
| Config keys or env vars added/changed | `docs/configuration.md` |
| Architecture, package layout, or data flow changed | `docs/architecture.md` and/or relevant ADR |
| Setup, quickstart, or project overview changed | `README.md` |

*Important:* Do NOT write tests for testing the presence or the content of the docs or PRDs.

**Instructions:**

1. Review the spec you just implemented and identify every user-facing or
   developer-facing surface that changed (API routes, request/response
   schemas, CLI parameters, environment variables, architectural decisions).
2. Open each affected doc and update it to reflect the new state. Do not
   leave placeholder text like "TODO" or "TBD" — write the actual content.
3. If a doc file listed above does not exist yet, create it with the correct
   content rather than skipping the update. A repository without a REST API or
   CLI keeps the file anyway and says in one line that it has none.
4. Run `make check` after doc updates to ensure nothing is broken.

## Documentation Style

Write every document for its reader, not for the code's authors. These rules
apply to every README and to every document in `docs/`, new or rewritten.

**READMEs** (the root `README.md`, and the `README.md` of a nested module or
an example) introduce; they do not explain internals. Each one, in order:

1. Says clearly **what the thing is, what it does and what problem it
   solves**, in a few sentences a newcomer understands.
2. Where appropriate, gives a **short example** of how to use it: a minimal
   code snippet or the command to run, and what to expect.
3. Points to **where to read more** (the user guide, the reference docs, the
   examples), and, for the root README, **how to install** and **how to
   develop**.

Implementation details a reader does not need in order to use the thing —
internal package names, protocol or wire details, spec and requirement ids,
errata, test names, the history of a decision — do **not** belong in a README.
Put them in `docs/` (`docs/architecture.md`, `docs/configuration.md`, or a
topic file such as `docs/{topic}.md`) and link to them.

**Reference documents** that cover a long list of things (`docs/api.md`,
`docs/cli.md`, `docs/configuration.md`, `docs/architecture.md`, and any other
document with more than a handful of sections) open with a short statement of
what the document covers and who it is for, followed by a **Contents** index
of links to its sections, so a reader can jump straight to what they need.
Keep the index in step with the headings whenever a section is added,
renamed or removed.

**General rules:**

- Lead with purpose, then usage, then detail.
- Every code snippet must compile against the current API and run as shown.
- Prefer one canonical place for each fact and link to it, rather than
  repeating it; when a fact must appear twice, keep both copies in step.
- Archived and historical records (`docs/prd/`, `docs/adr/`,
  `docs/archive/`, `.specs/`) are not rewritten to follow these rules.

## Tests and Spec Traceability

Specs are the input to an implementation, not the shape of its tests or its
comments. Once a spec is implemented, the code, its tests and the ADRs are
the record; the spec is archived.

- **Tests are organised by behaviour, not by spec.** A test file is named for
  the area of the production code it tests (`walk_test.go`, `edit_test.go`,
  `auth_test.go`), and a test for the behaviour it checks
  (`TestEdit_RejectsAnAmbiguousMatch`). Do not create per-spec test files,
  smoke, property, phase or group files, or test names and comments that
  carry spec or test-case ids (`TS_01_17`, `01-REQ-4.1`). The variants of one
  behaviour are one table-driven test.
- **Every behaviour is tested once**, at the level where it lives: a handler
  through the router, a store function directly, a pure function directly.
  Keep end-to-end tests (a real server, a real process, real credentials) for
  what only that layer proves. The test code of a package stays near the size
  of its production code.
- **Use the shared helpers** (`internal/testutil` in this repository) for what
  many packages' tests need: in a service the database, credentials,
  requests, responses and seeds; in a library the workspaces, scripted models
  and fixtures. A package keeps one `helpers_test.go` for what is specific to
  it. Create another package's tables with that package's schema function,
  never by copying its DDL.
- **Do not test artefacts**: no tests of an import graph, a build tag, the
  presence of a helper, a struct's fields, a constant's value, a
  documentation file's content, or that a package boundary holds.
- **Production comments give the reason, not a citation.** No spec or
  requirement ids in code comments; where a decision is the reason, cite the
  ADR (`docs/adr/`).
- **Where the code diverges from a spec, record the decision in the ADR of
  the feature** (or a new ADR), as a rule the code follows, and update the
  reference docs. Do not write errata.

## Generated Docs

`make doc` writes `go doc -all` output for every publicly accessible package of
the repository (no `internal/` packages, no `examples/`) to `docs/generated/`,
one text file per package. The files are committed to git.

Their purpose is orientation: **when you start working on this repository, use
`docs/generated/` to find your way around.** It is the cheapest way to see which
packages exist, what each one is for and which types, functions and constants
it exports, before reading any source. Prefer it to scanning directories or
grepping for declarations.

- Never edit these files by hand; they are overwritten by `make doc`.
- Treat them as possibly stale: the code is authoritative. If a file disagrees
  with the source, regenerate it.
- Run `make doc` and commit the result whenever a change adds, removes or
  alters an exported identifier or a package doc comment.

## Repository Specifics

