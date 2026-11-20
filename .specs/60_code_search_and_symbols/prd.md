---
spec_id: "60"
spec_name: "code_search_and_symbols"
title: "Search code and find symbols on the hub"
status: "active"
created_at: "2026-11-20T20:58:40Z"
intent_hash: "ce3396968454a519cde42a59d35e1588720e79351588673677897c6bb01ca07a"
contract_hash:
  requirements.json: "64633822c976840b31fdf3cf892b9c6fa660f07eb3539285d97fa33656d963eb"
  tasks.json: "9d19f5640f2fea3b44434bd9be9858b0e54da37cf9a95b0bc6384c40170acfbf"
schema_version: 3
source: "docs/prd/20_hub_code_index.md"
---

# Search code and find symbols on the hub

## Intent

An agent can find its way around a repository on the hub before it has a
checkout or a sandbox: ranked code search and symbol lookup over the
default branch of a workspace, answered from the index the hub keeps
current. A search never answers from a commit older than the default
branch's head at the time the request arrived, so what it finds is what a
fork made now would hold.

## Goals

1. `GET …/scm/code-search` answers a query over the default branch of a
   workspace from its index, ranked and grouped by file, and names the
   commit it searched.
2. `GET …/scm/symbols` finds declarations by name over the same index.
3. A search never answers from a commit older than the default branch's
   head at the time the request arrived: it waits for the index, and says
   so when the wait runs out.
4. A fork is searched through its parent's index, and the answer says so.
5. The two operations are `parley scm` commands, and so are tools of
   `parley mcp` without further work.

## Non-goals

- The index itself: configuration, the setting, the tree, the builds, the
  status route and the build metrics. It is package 59.
- `GET …/scm/outline`. It is package 58.
- Indexing or searching any ref but the default branch, or a fork's own
  branches. Work in progress is searched in its sandbox (PRD 18).
- Changing `GET …/scm/search`. It stays the exact, unindexed search at
  any ref.
- Finding references, type checking, or any analysis that needs a build.
- Search across workspaces in one request; semantic or embedding search.

## Background

Package 59 keeps, for every repository workspace that is enabled, an index
of the default branch's head built with agentkit-go's `codesearch`,
records the indexed commit, queues a build whenever the head moves, and
shows the state at `GET …/scm/code-index`.

The index answers two kinds of question. Its `code_search` tool takes a
query in zoekt's syntax and returns files in rank order, each with its
match count and up to three chunks of lines, at most 25 files a call, for
a query of at most 1,024 bytes, within 10 seconds. Its `Symbols` method
answers a name lookup from the outlines collected at build time: a prefix
match that ignores case unless the name has an upper-case letter, or an
exact one, filtered by kind and path. It answers only from an index that
is complete, and only when at most 1,000 declarations match.

The source control reads share `[scm].read_timeout` (default 30 s) and
answer `503 scm_timeout` past it. `GET …/scm/search` (SCM-FR-009) is
exact and unranked, at any ref.

## Requirements

### Waiting for the index

Search and symbols answer only from an index built from the head of the
default branch as it was when the request arrived.

- When the index is at that commit, the answer is immediate.
- Otherwise the request waits for the build, up to `[scm].read_timeout`.
  When the build finishes in time, the request is answered from it.
- When the wait runs out, or the last build of that commit failed, the
  answer is `503`, `error_type: code_index_not_ready`, with
  `Retry-After` and, in the error object, `state` (`building` or
  `failed`), `head_sha` and `indexed_sha` (null when there is none).
- Every answer carries `indexed_sha`, and `partial: true` when the
  index is partial.

### Forks

A fork has no tree and no index. Search and symbols addressed to a fork
answer from its parent's index, under the same waiting rule applied to the
parent's default branch, and say so: the answer carries `workspace`, the
parent's slug, next to `indexed_sha`. Read access to the fork is what is
needed, as for the fork's other reads (SCM-FR-004).

### The routes

Both need `git:read` for PATs and read access to the workspace
(SCM-FR-001), and follow the conventions of §3.

`GET /workspaces/:slug/scm/code-search`

- Query: `q` (required, 1 to 500 characters and at most 1,024 bytes,
  zoekt query syntax), `limit` (files, default 20, 1 to 25).
- `200`:
  `{"workspace", "indexed_sha", "partial", "truncated", "files": [{"path", "language", "matches": [{"line", "text"}]}]}`,
  files in rank order, at most 20 matches per file, `text` cut to 500
  characters. `truncated` is true when more files matched than `limit`.
- A query the index cannot parse is `400`, `error_type: invalid_query`,
  with the parser's message.

`GET /workspaces/:slug/scm/symbols`

- Query: `name` (required, 1 to 200 characters; a prefix match that
  ignores case unless the name has an upper-case letter), `exact` (bool),
  `kind`, `path` (a directory or file to stay under), `limit` (default
  20, 1 to 50).
- `200`:
  `{"workspace", "indexed_sha", "partial", "truncated", "symbols": [{"path", "kind", "name", "container", "signature", "exported", "start_line", "end_line"}]}`.

For both: with the feature off, `404`, `error_type: code_index_disabled`;
with the workspace's setting off, `409`, `error_type: code_index_off`.
Both answers carry an `ETag` derived from `indexed_sha` and the query,
and honour `If-None-Match`.

### CLI

Two commands, declared like every command (ADR 02), each printing the
route's answer:

- `parley scm code-search <slug> <query>` with `--limit`.
- `parley scm symbols <slug> <name>` with `--exact`, `--kind`,
  `--path`, `--limit`.

### Metrics

`parley_code_index_wait_seconds`: how long queries waited for the index.

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
4. **An oversized repository.** A workspace over `max_files` gets a
   partial index; its searches answer with `partial: true`.

## Design Decisions

1. **A fork answers from its parent's index** (owner's decision, PRD 20),
   and names the parent in the answer so the result is not mistaken for
   the fork's own state.
2. **New routes and CLI commands; `scm/search` is untouched** (owner's
   decision, PRD 20).
3. **A search waits for the index of the current head** (owner's decision,
   PRD 20), bounded by the read timeout all source control reads share.
4. **Constraint: search and symbols come from agentkit-go's
   `codesearch`**, not from new code in Parley (PRD 20, decision 10).
5. **This package is the third scope of PRD 20.**
6. **`limit` of code search is 1 to 25**, not PRD 20's 1 to 100:
   agentkit-go's `code_search` returns at most 25 files a call and has no
   other search entry point, and decision 4 rules out a second one. The
   default stays 20.
7. **Matches are the matching lines of the best chunks.** The hub asks
   the tool for no context lines and answers the lines it marks as
   matches, in line order, at most 20 a file. The tool shows at most three
   chunks a file, so a file with matches in more places shows its best
   three; `matches` is never empty for a file that is answered.
8. **`language` is agentkit-go's name for the file's extension**
   (`Go`, `Python`, `C++`), as on the outline route, and `""` for a
   file of no known language.
9. **An index newer than the arrival head answers.** The rule forbids an
   older commit: when the head moved again during the wait and the index
   is at the newer head, the request is answered from it.
10. **A waiting request makes sure a build is on its way.** A request
    that finds the index behind the head and no build pending queues one,
    as package 59's maintenance pass would, and then waits.
11. **`Retry-After`** is 5 seconds while a build is running or queued,
    and the seconds until the failed build's next attempt, at least 1,
    when the last build of the head failed. A failed build of the head is
    answered at once, without waiting.
12. **Symbols need a complete index.** agentkit-go answers symbols only
    from an index that is not partial. A symbols request to a partial
    index is `409`, `error_type: code_index_partial`, naming
    `indexed_sha`; code search answers from it with `partial: true`.
    This narrows PRD 20, which says a partial index answers queries.
13. **A name that matches more than 1,000 declarations is refused**:
    `422`, `error_type: too_many_matches`, saying to give a longer
    name, a kind or a path. agentkit-go does not answer such a lookup.
14. **A search the index does not finish in its own 10 seconds** is
    `503`, `error_type: scm_timeout`, with the index's advice to narrow
    the query.
15. **A workspace without a head** (no repository yet, an empty
    repository) answers as `GET …/scm/tree` without a ref answers for
    it.
16. **The checks come in the order**: credential and access, the feature
    (`404`), the parameters (`400`), the setting (`409`), the head,
    the waiting rule (`503`), the entity tag (`304`), the query.
    A fork takes the setting and the head of its parent.
17. **The wait metric is a histogram observed once per request that
    reached the waiting rule**, 0 for a request that did not wait, with
    the label `outcome` (`ready`, `timeout`, `failed`).
18. **Symbols are answered in the index's order**, cut to `limit`;
    `truncated` is true when more matched.
19. **Profile `full`.**

## Dependencies

- Package 59 `code_index_builds`: the index, its record of the indexed
  commit, its builds and its state.
- Package 20 `scm_read_api`: the read conventions, the timeout and the
  caching helpers.
- Package 15 `task_fork_creation`: a fork and its parent.
- Package 58 `scm_file_outline`: the outline command of the first flow,
  and the language names.
- Package 09 `cli_foundation` and ADR 02; package 57 `cli_mcp_server`,
  which serves the new commands as tools.
- agentkit-go `codesearch`, as package 59 takes it.

## Verified External API

`github.com/agent-fox-dev/agentkit-go/codesearch`, read in its source at
agentkit-go's commit `20c16c9`:

- `(*Index).Tools() []core.Tool` returns one tool, `code_search`; the
  package exports no other search entry point. Its input is
  `{"query": string, "path"?: string, "max_files"?: int, "context_lines"?: int}`:
  a query that is empty or over 1,024 bytes and one zoekt cannot parse
  are the error `invalid_arguments`, the latter with the message
  `query parse error: …`; `max_files` defaults to 10 and is clamped to
  25; `context_lines` 0 means no context.
- The tool's result data is
  `{"files": [{"path", "score", "matches", "chunks": [{"lines": [{"line", "text", "match"}]}], "symbols"}], "truncated", "note", "partial", "partial_reason", "symbol_sources", "files_indexed", "dirty_files", "skipped"}`,
  files in score order with at most three chunks each.
- Its other errors: `aborted` (the context ended), `index_failed`,
  `index_closed`, `search_failed` (a query that outlasts 10 s says
  `query timed out; narrow the query or add a path constraint`).
- `(*Index).Symbols(ctx, tools.SymbolQuery) (tools.SymbolAnswer, bool, error)`;
  `SymbolQuery{Name, Kind, Path string; Exact bool}`, `Kind` lower-case
  and `Path` a slash-separated directory relative to the tree;
  `SymbolAnswer{Matches []SymbolMatch; FilesIndexed int; Backends map[string]int}`;
  `SymbolMatch{Path, Backend, Kind, Name, Container, Signature string; Exported bool; StartLine, EndLine int}`.
  The bool is false when the index is not built or loaded, is partial or
  closed, or more than 1,000 declarations match.
- `core.Tool.Execute(ctx, json.RawMessage) core.ToolResult` and
  `outline.LangFor(path string, src []byte) string` in agentkit-go
  v0.10.0.

**Unverified**: the order of `SymbolAnswer.Matches`; whether
`SymbolQuery.Path` may name a file as well as a directory; how the text
of a line in a chunk is cut by the tool itself.

## Glossary

- **Index**, **indexed commit**, **partial index**, **build**,
  **enabled**: as in package 59.
- **Arrival head**: the head commit of the default branch at the time a
  request arrived.
- **Waiting rule**: a search answers only from an index of the arrival
  head or a later one, and waits for it up to the read timeout.
