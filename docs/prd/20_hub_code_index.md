# Search code, find symbols and outline files on the hub

## Intent

An agent can find its way around a repository on the hub before it has a
checkout or a sandbox: ranked code search and symbol lookup over the
default branch, answered from an index the hub keeps current, and the
outline of any file at any ref. Orientation, the part of a task that reads
the most and changes nothing, then costs one request and no clone.

## Goals

1. `GET …/scm/code-search` answers a query over the default branch of a
   workspace from an index, ranked and grouped by file, and names the
   commit it searched.
2. `GET …/scm/symbols` finds declarations by name over the same index.
3. `GET …/scm/outline` returns the declarations of one file at any ref,
   with no index involved.
4. A search never answers from a commit older than the default branch's
   head at the time the request arrived: it waits for the index, and says
   so when the wait runs out.
5. After a hub restart, a search of a workspace whose default branch did
   not move answers without a rebuild.
6. The same three operations are `parley scm` commands, and so are tools
   of `parley mcp` (PRD 19) without further work.
7. A hub without `[code_index]` in its configuration builds nothing and
   uses no disk for it; the outline route works all the same.

## Non-goals

- Indexing any ref but the default branch, or a fork's own branches. Work
  in progress is searched in its sandbox (PRD 18).
- Changing `GET …/scm/search`. It stays the exact, unindexed search at any
  ref.
- Finding references, type checking, or any analysis that needs a build.
- Search across workspaces in one request.
- Semantic or embedding search.
- Events for index builds. A build is not a state change a consumer acts
  on; it has metrics and a status route.
- Indexing from git objects without a tree on disk. The owner's decision
  is to materialise the default branch.

## Background

The source control API (PRD §6, packages 20 and 21) reads a workspace's
bare repository: refs, commits, trees, files, diffs, blame, and
`GET …/scm/search` (SCM-FR-009), which runs a literal or regular-expression
match over the files of one ref with `git grep` (`internal/gitrepo/grep.go`,
`internal/scm/search.go`). It is exact and works at any ref, and it reads
every candidate file on every request, returns matches in path order, and
knows nothing about declarations. All reads share `[scm].read_timeout`
(default 30 s) and answer `503 scm_timeout` past it.

agentkit-go has what is missing:

- `codesearch` (a nested module on zoekt): a trigram index over a
  directory with a ranked, file-grouped `code_search` tool that takes
  zoekt's query syntax, and symbol lookup from the outlines collected at
  build time. A build is bounded by a file count (100,000), a byte count
  (1 GiB) and a wall time (60 s); when a bound stops it, the index keeps
  what it has and marks itself partial.
- `outline`: the declarations of one source file (kind, name, container,
  signature, exported, start and end line) for fifteen languages, parsed
  with tree-sitter from the file's bytes alone.

Both need what the hub does not have today. The index is built from a
directory, and the hub holds bare repositories. Tree-sitter is C, and
`parleyd` is built without cgo today; the owner accepted cgo in the hub.
The index lives for one process today; agentkit-go plans a package,
`persistent_code_index`, that keeps it on disk between processes (its
PRD 15).

The hub already has what a background build needs: the job queue of
ADR 09 (one pending job per type and workspace, retried, surviving a
restart), the stages that run when a ref moves (ADR 08), workspace
settings (PRD §4.3.3, package 11), and the removers that run when a
workspace is deleted (ADR 07).

## Requirements

### Configuration and the workspace setting

The feature is on when the hub's configuration has a `[code_index]`
section.

| Key | Default | Meaning |
|---|---|---|
| `max_files` | 100000 | Most files one index holds |
| `max_size` | `"1GB"` | Most bytes of content one index holds |
| `build_timeout` | `"5m"` | Longest one build may run |
| `workers` | 2 | Builds that run at once, 1 to 16 |

- Static validation refuses an unknown key, a `workers` outside 1 to 16
  and a size or duration it cannot read, each with a message naming the
  key, together with the hub's other configuration failures.
- On a platform where the index cannot run, a hub with `[code_index]`
  refuses to start with one message saying so.
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
- A build that the bounds stop leaves a **partial** index, which answers
  queries and says so. A build that fails for another reason leaves the
  previous index in place for the status route to report, is retried with
  the queue's growing delays, and is retried at once when the head moves.
- An archived workspace keeps its index. A deleted workspace's tree, index
  and pending builds are removed with it.
- The tree and the index live under the hub's data directory and survive
  a restart.

### Waiting for the index

Search and symbols answer only from an index built from the head of the
default branch as it was when the request arrived.

- When the index is at that commit, the answer is immediate.
- Otherwise the request waits for the build, up to `[scm].read_timeout`.
  When the build finishes in time, the request is answered from it.
- When the wait runs out, or the last build of that commit failed, the
  answer is `503`, `error_type: code_index_not_ready`, with `Retry-After`
  and, in the error object, `state` (`building` or `failed`), `head_sha`
  and `indexed_sha` (null when there is none).
- Every answer carries `indexed_sha`, and `partial: true` when the index
  is partial.

### Forks

A fork has no tree and no index. Search, symbols and the status route
addressed to a fork answer from its parent's index, under the same waiting
rule applied to the parent's default branch, and say so: the answer
carries `workspace`, the parent's slug, next to `indexed_sha`. Read access
to the fork is what is needed, as for the fork's other reads
(SCM-FR-004). Outline on a fork reads the fork's refs like any SCM read.

### The routes

All four need `git:read` for PATs and read access to the workspace
(SCM-FR-001), and follow the conventions of §3.

`GET /workspaces/:slug/scm/code-search`

- Query: `q` (required, 1 to 500 characters, zoekt query syntax), `limit`
  (files, default 20, 1 to 100).
- `200`: `{"workspace", "indexed_sha", "partial", "truncated", "files": [{"path", "language", "matches": [{"line", "text"}]}]}`,
  files in rank order, at most 20 matches per file, `text` cut to 500
  characters. `truncated` is true when more files matched than `limit`.
- A query the index cannot parse is `400`, `error_type: invalid_query`,
  with the parser's message.

`GET /workspaces/:slug/scm/symbols`

- Query: `name` (required, 1 to 200 characters; a prefix match that
  ignores case unless the name has an upper-case letter), `exact` (bool),
  `kind`, `path` (a directory or file to stay under), `limit` (default
  20, 1 to 50).
- `200`: `{"workspace", "indexed_sha", "partial", "truncated", "symbols": [{"path", "kind", "name", "container", "signature", "exported", "start_line", "end_line"}]}`.

`GET /workspaces/:slug/scm/outline`

- Query: `path` (required), `ref` (default: the default branch).
- `200`: `{"path", "resolved_sha", "language", "supported", "declarations": [{"kind", "name", "container", "signature", "exported", "start_line", "end_line"}]}`,
  declarations in line order. A file in a language without a grammar
  answers `supported: false` with no declarations.
- Errors: `404 path_not_found`; `400` when `path` is a directory;
  `413 payload_too_large` for a file over 1 MiB; `422`,
  `error_type: binary_file`; `503 scm_timeout`.
- It works with the feature off and with the setting off, on any
  workspace and ref the file route serves, and is cached as SCM-FR-002
  says.

`GET /workspaces/:slug/scm/code-index`

- `200`: `{"workspace", "enabled", "state", "head_sha", "indexed_sha", "partial", "files_indexed", "built_at", "error"}`,
  with `state` one of `off`, `queued`, `building`, `ready`, `failed`.
  `error` is the last build's failure, scrubbed and cut to 1,024 bytes.

For search and symbols: with the feature off, `404`,
`error_type: code_index_disabled`; with the workspace's setting off,
`409`, `error_type: code_index_off`. Both answers carry an `ETag` derived
from `indexed_sha` and the query, and honour `If-None-Match`.

### CLI

Four commands, declared like every command (ADR 02), each printing the
route's answer:

- `parley scm code-search <slug> <query>` with `--limit`.
- `parley scm symbols <slug> <name>` with `--exact`, `--kind`, `--path`,
  `--limit`.
- `parley scm outline <slug> <path>` with `--ref`.
- `parley scm code-index <slug>`.

### Metrics

New: `parley_code_index_builds_total` by outcome (`ok`, `partial`,
`failed`), `parley_code_index_build_duration_seconds`,
`parley_code_index_bytes` (trees and indexes on disk), and
`parley_code_index_wait_seconds` (how long queries waited).

### End-to-end flows

1. **Orientation without a clone.** An agent calls
   `parley scm code-search demo 'sym:Handler lang:go'`, then
   `parley scm symbols demo NewRouter`, then `parley scm outline demo
   internal/api/router.go`, and creates its fork knowing where to work.
2. **A land, then a search.** A change lands on `demo`. A search arriving
   one second later waits for the build of the new head and answers with
   `indexed_sha` equal to the landed commit.
3. **A fork.** `parley scm code-search demo--greeting greet` answers from
   `demo`'s index with `workspace: "demo"`.
4. **A restart.** The hub restarts; the first search of an unchanged
   workspace answers without a build.
5. **An oversized repository.** A workspace over `max_files` gets a
   partial index; its searches answer with `partial: true`.

## Design Decisions

1. **Materialise the default branch** (owner's decision): the index is
   built from a tree on disk, which is what agentkit-go's index reads.
2. **Every repository workspace is indexed, with a setting to turn one
   off** (owner's decision).
3. **A fork answers from its parent's index** (owner's decision), and
   names the parent in the answer so the result is not mistaken for the
   fork's own state.
4. **New routes and CLI commands; `scm/search` is untouched** (owner's
   decision). Two engines behind one route would match differently.
5. **A search waits for the index of the current head** (owner's
   decision), bounded by the read timeout all SCM reads share.
6. **cgo in the hub** (owner's decision). Consequence stated plainly:
   `parleyd` needs a C toolchain to build, and repository content is
   parsed by C code in the hub's process, bounded by the 1 MiB file limit
   and the build timeout. The `parley` CLI is not affected.
7. **Outline needs no index and no setting**: it parses one file's bytes,
   so it works at any ref and with the feature off.
8. **`build_timeout` defaults to 5 minutes**, not agentkit-go's 60 s: the
   hub builds in the background, and a partial index is worse than a
   slower one.
9. **No events**: a consumer that cares uses the status route or the
   waiting rule.
10. **Constraint: search, symbols and outlines come from agentkit-go**
    (`codesearch`, `outline`), not from new code in Parley.
11. **A split is expected**: outline alone; the tree, the builds and the
    status route; then search and symbols with the waiting rule and the
    fork rule.

## Dependencies

- Packages 20 `scm_read_api` and 11 `workspace_read_update_settings`
  (active, done): the read conventions and the settings this extends.
- ADR 07, ADR 08, ADR 09: removal with the workspace, the stage on a ref
  move, and the job queue.
- agentkit-go `codesearch` and `outline` (built), and its planned
  `persistent_code_index` package (not built): goal 5 needs it. Without
  it everything else works and a restart rebuilds every index.
- PRD 19 (`parley mcp`, not built): serves the new commands as tools
  because they are in the `scm` group. No dependency in either direction.

## Verified External API

`github.com/agent-fox-dev/agentkit-go`, read in its checkout
(`docs/generated/`):

- `workspace.Open(root string, opts workspace.Options) (*workspace.Workspace, error)`;
  `workspace.NoGlobalExcludes()` gives options that read no ignore file
  outside the tree.
- `codesearch.New(ws *workspace.Workspace, opts codesearch.Options) (tools.Index, error)`;
  `Options` has `MaxFiles int`, `MaxBytes int64`,
  `MaxBuildTime time.Duration`, `TempDir string`. It returns
  `codesearch.ErrUnsupported` where zoekt cannot run.
- `(*codesearch.Index).Build(ctx) error`, `BuildStats()`,
  `IndexedFiles() map[string]bool`, `Close() error` (which deletes the run
  directory today).
- `(*codesearch.Index).Symbols(ctx, tools.SymbolQuery) (tools.SymbolAnswer, bool, error)`;
  `SymbolQuery` has `Name`, `Kind`, `Path`, `Exact`; `SymbolMatch` has
  `Path`, `Backend`, `Kind`, `Name`, `Container`, `Signature`, `Exported`,
  `StartLine`, `EndLine`. It answers only from a complete, built index.
- `(*codesearch.Index).Tools() []core.Tool` returns the `code_search`
  tool; the package exports no other search entry point.
- `outline.Outline(ctx, abs string, src []byte, opts outline.Options) (outline.File, error)`;
  `File` has `Path`, `Lang`, `Backend`, `Decls`; `Decl` has `Kind`,
  `Name`, `Container`, `Signature`, `Exported`, `StartLine`, `EndLine`;
  `Options.MaxFileBytes` defaults to 1 MiB.

**Unverified**: the `code_search` tool's parameters and the shape of its
result (the route's answer above is what the hub promises, whatever
mapping that takes); how `Outline` reports a language without a grammar;
whether the index's walk skips dot files and files the tree's own ignore
files name, which would leave them out of search. **Not built**:
`persistent_code_index`.

## Glossary

- **Tree**: the files of a workspace's default-branch head, kept on disk
  for indexing.
- **Index**: the search index and symbol data built from a tree.
- **Indexed commit** (`indexed_sha`): the commit an index was built from.
- **Partial index**: an index a bound stopped; it holds some of the files.
- **Build**: one job that brings a workspace's tree and index to the
  current head.
