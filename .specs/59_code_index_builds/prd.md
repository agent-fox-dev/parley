---
spec_id: "59"
spec_name: "code_index_builds"
title: "Keep a code index of every workspace's default branch"
status: "active"
created_at: "2026-11-20T20:55:55Z"
intent_hash: "2b6542c5285741f3a314166ddcbbf8079f5b0ed81ae5b4a0ac0e1862ce1ee083"
contract_hash:
  requirements.json: "1f0f1ce22ba15053b2956a0fe5dcfdca2f8cdbe095f5025f1d779cb2a454d318"
  tasks.json: "6c114e3deee2659f2355d7cbb283045dd1a79c1bcdee8579a67718a02dc5faea"
schema_version: 3
source: "docs/prd/20_hub_code_index.md"
---

# Keep a code index of every workspace's default branch

## Intent

The hub keeps, for every repository workspace, a search index of the
default branch's head commit and brings it up to date whenever that head
moves, in the background, so that a later search finds it ready. An
operator turns the feature on with one configuration section, a workspace
owner turns one workspace off with a setting, and anyone who may read a
workspace sees with one request which commit its index holds.

## Goals

1. A hub with `[code_index]` in its configuration keeps a tree and an
   index of the default branch's head for every repository workspace whose
   setting is on.
2. The index follows the head: a build is queued whenever the head of the
   default branch moves, by any route, and at most one is pending per
   workspace.
3. `GET …/scm/code-index` and `parley scm code-index` say which commit a
   workspace's index holds and what state it is in.
4. After a hub restart, a workspace whose default branch did not move has
   its index without a rebuild.
5. A hub without `[code_index]` builds nothing and uses no disk for it.
6. A workspace's tree, index and pending builds go with the workspace when
   it is deleted, and when its setting is turned off.

## Non-goals

- Searching the index: `GET …/scm/code-search`, `GET …/scm/symbols`,
  the rule that a search waits for the index, and their CLI commands and
  the wait metric. It is the third scope of PRD 20, the next package
  (`code_search_and_symbols`).
- `GET …/scm/outline`. It is package 58 and needs none of this.
- Indexing any ref but the default branch, or a fork's own branches.
- Indexing from git objects without a tree on disk.
- Events for index builds. A build has metrics and the status route.
- Search across workspaces, semantic or embedding search, finding
  references.

## Background

agentkit-go's `codesearch` (a nested module on zoekt) builds a trigram
index over a directory, with the outlines of its files collected at build
time. A build is bounded by a file count, a byte count and a wall time;
when a bound stops it, the index keeps what it has and marks itself
partial. With `Options.IndexDir` a completed build is stored on disk and
loaded by a later process on the same directory. The index reads the
directory's own ignore files.

The hub holds bare repositories, so it has no directory to index. It has
what a background build needs: the job queue of ADR 09 (one pending job
per type and workspace, a follow-up beside a running one, retried with
growing delays, surviving a restart, removed with the workspace), the
listeners told after every accepted write of refs (ADR 08), the
maintenance loop, the workspace settings registry (package 11) and the
removers that run when a workspace is deleted (ADR 07). The head of a
workspace's default branch is also written outside the ref writer: by a
sync, a reclone, a rebuild of a carry-patch workspace and the first clone.

Package 58 put cgo into the hub image's `parleyd`, which the outlines an
index collects for languages other than Go need.

## Requirements

### Configuration

The feature is on when the hub's configuration has a `[code_index]`
section.

| Key | Default | Meaning |
|---|---|---|
| `max_files` | 100000 | Most files one index holds |
| `max_size` | `"1GB"` | Most bytes of content one index holds |
| `build_timeout` | `"5m"` | Longest one build may run |
| `workers` | 2 | Builds that run at once, 1 to 16 |

- Static validation refuses an unknown key, a `max_files` below 1, a
  `workers` outside 1 to 16 and a size or duration it cannot read, each
  with a message naming the key, together with the hub's other
  configuration failures.
- On a platform where the index cannot run, a hub with `[code_index]`
  refuses to start with one message saying so.

### The workspace setting

- A repository workspace gains the setting `code_index` (bool, default
  `true`). A fork shows its parent's value and cannot change it, as for
  every setting.
- Turning the setting off removes the workspace's tree and index within
  one maintenance pass; turning it on queues a build.

### What is indexed, and when

For every repository workspace with the setting on, the hub keeps a
**tree**, the files of the default branch's head commit on disk, and the
**index** built from it.

- The tree holds the commit's files and nothing else: no `.git`, no
  submodule contents, no hook is run and no filter is applied; a symbolic
  link is kept as a link and never followed.
- A build is queued when the workspace is created and has its repository,
  whenever the head of its default branch moves by any route (a land, a
  push, a sync, an API commit or ref move, a rebuild, a reclone), when the
  setting is turned on, and at hub start for every workspace whose index
  is not at its head.
- There is at most one pending build per workspace. A build always takes
  the head as it is when it starts; a head that moved while it ran queues
  the next one.
- A build that the bounds stop leaves a **partial** index, which says so.
  A build that fails for another reason leaves the previous index in
  place for the status route to report, is retried with the queue's
  growing delays, and is retried at once when the head moves.
- An archived workspace keeps its index. A deleted workspace's tree, index
  and pending builds are removed with it.
- The tree and the index live under the hub's data directory and survive
  a restart.

### The status route

`GET /workspaces/:slug/scm/code-index` needs `git:read` for PATs and
read access to the workspace (SCM-FR-001), and follows the conventions
of §3.

- `200`:
  `{"workspace", "enabled", "state", "head_sha", "indexed_sha", "partial", "files_indexed", "built_at", "error"}`,
  with `state` one of `off`, `queued`, `building`, `ready`,
  `failed`. `error` is the last build's failure, scrubbed and cut to
  1,024 bytes.
- Addressed to a fork it answers for the fork's parent, and says so:
  `workspace` is the parent's slug.

### CLI

`parley scm code-index <slug>`, declared like every command (ADR 02),
prints the route's answer.

### Metrics

`parley_code_index_builds_total` by outcome (`ok`, `partial`,
`failed`), `parley_code_index_build_duration_seconds` and
`parley_code_index_bytes` (trees and indexes on disk).

### End-to-end flows

1. **A land, then the index.** A change lands on `demo`. Shortly after,
   `parley scm code-index demo` answers `state: "ready"` with
   `indexed_sha` equal to the landed commit.
2. **A restart.** The hub restarts; the status of an unchanged workspace
   is `ready` at the same commit and no build ran.
3. **An oversized repository.** A workspace over `max_files` gets a
   partial index; its status says `partial: true`.
4. **Turned off.** A developer sets `code_index` to false on `demo`;
   after a maintenance pass the status is `off` and nothing of `demo`
   is left under the hub's code index directory.

## Design Decisions

1. **Materialise the default branch** (owner's decision, PRD 20): the
   index is built from a tree on disk, which is what agentkit-go's index
   reads.
2. **Every repository workspace is indexed, with a setting to turn one
   off** (owner's decision, PRD 20).
3. **`build_timeout` defaults to 5 minutes**, not agentkit-go's 60 s
   (PRD 20, decision 8).
4. **No events** (PRD 20, decision 9).
5. **Constraint: the index comes from agentkit-go's `codesearch`**, not
   from new code in Parley (PRD 20, decision 10).
6. **This package is the second scope of PRD 20**; the third is named
   under Non-goals.
7. **The maintenance pass reconciles.** Each pass queues a build for
   every indexed workspace whose index is not at its head and has no
   pending build, and removes the tree and index of every workspace that
   no longer exists or whose setting is off. The routes that move a head
   queue a build themselves; the pass is what makes "by any route" hold
   for a route that is added later or whose notice was lost, and it is
   the same work as the pass at hub start.
8. **The status route answers with the feature off**: `200` with
   `enabled: false` and `state: "off"`, as for a workspace whose
   setting is off. `enabled` is true exactly when the hub has
   `[code_index]` and the workspace's setting is on. The refusals
   `code_index_disabled` and `code_index_off` belong to search and
   symbols, in the next package.
9. **What `state` means.** `off`: not enabled. `building`: a build is
   running. `queued`: enabled, no build running, and the index is not at
   the head with a build pending or due, a workspace without a repository
   yet included. `failed`: the last build of the current head failed and
   none is running. `ready`: the index is at the head. `indexed_sha`,
   `partial`, `files_indexed` and `built_at` always describe the
   index there is, in every state, and are null (`partial` false,
   `files_indexed` 0) when there is none.
10. **A fork's status is its parent's** (PRD 20, decision 3, applied to
    this route): a fork has no tree, no index and no build of its own.
11. **`workers` bounds the builds, not the queue.** At most `workers`
    builds run at once, whatever else the job queue runs; a build that
    finds no free worker waits as a queued job.
12. **A build has the job type `code_index`** in the queue of ADR 09.
    A build stopped by the queue because its workspace was deleted or the
    hub is stopping counts as no outcome in the metric.
13. **The index leaves out what the tree's own ignore files name**, as
    agentkit-go's index does: a committed file that a committed
    `.gitignore` matches is not indexed. No ignore file outside the tree
    is read.
14. **`parley_code_index_bytes` is measured at the end of every build
    and removal and at hub start**, not on every scrape.
15. **A build that exceeds `build_timeout` leaves a partial index**, as a
    build stopped by `max_files` or `max_size` does; it counts as
    `partial`.
16. **The persistent index needs a release of agentkit-go's
    `codesearch` after `codesearch/v0.9.0`.** `Options.IndexDir` is on
    agentkit-go's main branch and in no tagged release of the module yet;
    until one is tagged, `go.mod` takes the module at a commit.
17. **Profile `full`.**

## Dependencies

- Package 20 `scm_read_api`: the read conventions and the route helper
  the status route uses.
- Package 11 `workspace_read_update_settings`: the settings registry the
  new setting is registered with.
- Package 33 `rebuild_jobs` and ADR 09: the job queue.
- Packages 12, 14, 25 and 34: the sync, the reclone, archive and delete,
  the land and the rebuild, each of which moves a head or ends a
  workspace.
- Package 03 `server_lifecycle_and_configuration`: configuration
  sections, static validation and the start stages.
- Package 09 `cli_foundation` and ADR 02; package 58
  `scm_file_outline`: the cgo build of `parleyd`.
- ADR 07, ADR 08: removal with the workspace, and the listeners of a ref
  write.
- agentkit-go `codesearch`, at its main branch (see decision 16).

## Verified External API

`github.com/agent-fox-dev/agentkit-go/codesearch`, read with `go doc`
and in its source at agentkit-go's commit `20c16c9` (main; after the tag
`codesearch/v0.9.0`, which has no `IndexDir`):

- `codesearch.New(ws *workspace.Workspace, opts codesearch.Options) (tools.Index, error)`;
  the result asserts to `*codesearch.Index`. It returns
  `codesearch.ErrUnsupported` where zoekt cannot run (windows).
- `Options`: `MaxFiles int` (0 means 100,000), `MaxBytes int64` (0
  means 1 GiB), `MaxBuildTime time.Duration` (0 means 60 s; a build may
  run up to 1.25 times it), `TempDir string`, `IndexDir string` (keeps
  the index of a workspace under
  `IndexDir/agentkit-codesearch-<sha256 of the workspace root>/`; must
  lie outside the workspace; `Close` keeps a stored index).
- `(*Index).Build(ctx) error`: a bound that stops it leaves the index
  partial; a cancelled context discards the build.
- `(*Index).BuildStats() BuildStatsResult` with `FilesIndexed int` and
  the counts of skipped files; `(*Index).IndexedFiles() map[string]bool`;
  `(*Index).Close() error`.
- `workspace.Open(root string, opts workspace.Options) (*workspace.Workspace, error)`
  and `workspace.NoGlobalExcludes() workspace.IgnoreOptions`, the ignore
  options that read no ignore file outside the tree, in
  `github.com/agent-fox-dev/agentkit-go/workspace` (v0.10.0).

**Unverified**: how an index tells a caller that it is partial other than
through the `code_search` tool's result (`partial`); whether a stored
index of a tree whose files changed in place is rebuilt or revalidated
file by file when it is loaded; whether the index's walk skips dot files.

## Glossary

- **Tree**: the files of a workspace's default-branch head, kept on disk
  for indexing.
- **Index**: the search index and symbol data built from a tree.
- **Indexed commit** (`indexed_sha`): the commit an index was built from.
- **Partial index**: an index a bound stopped; it holds some of the files.
- **Build**: one job that brings a workspace's tree and index to the
  current head.
- **Enabled**: the hub has `[code_index]` and the workspace's setting
  `code_index` is on.
