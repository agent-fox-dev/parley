---
spec_id: "58"
spec_name: "scm_file_outline"
title: "Outline a file of a workspace on the hub"
status: "active"
created_at: "2026-11-20T20:29:11Z"
intent_hash: "e097cc9fe89f8f7cfa0175d65d7cb4a0e964767bb6c8d178cdec50e0209ae849"
contract_hash:
  requirements.json: "0d04f2a278cddfdfc089becd48486d8bb68659a527c777979627496ef2723bce"
  tasks.json: "c08eaafd3a3cdc0844a72dde9d0d2b75500be1d5209d4760f67609c04ee3c338"
schema_version: 3
source: "docs/prd/20_hub_code_index.md"
---

# Outline a file of a workspace on the hub

## Intent

An agent can see what a source file of a workspace declares, at any ref,
with one request and no clone: the functions, types and other declarations
of the file, each with its kind, name, signature and lines. It is the first
part of finding one's way around a repository on the hub, and it needs no
index.

## Goals

1. `GET …/scm/outline` returns the declarations of one file at any ref the
   file route serves, in line order.
2. Files in fifteen languages are outlined; a file in any other language
   answers that it is not supported, not an error.
3. The route follows the conventions of the other source control reads:
   access, the read timeout, caching and the request metric.
4. `parley scm outline` prints the route's answer, and is a tool of
   `parley mcp` without further work.
5. The hub image holds a `parleyd` that outlines all fifteen languages;
   the `parley` CLI still builds without a C toolchain.

## Non-goals

- The code index: the `[code_index]` section, the workspace setting
  `code_index`, the tree on disk, the builds, the status route
  `GET …/scm/code-index` and the build metrics. It is the second scope of
  PRD 20, a package of its own (`code_index_builds`).
- Code search and symbol lookup over the index: `GET …/scm/code-search`,
  `GET …/scm/symbols`, the waiting rule and the fork rule. It is the third
  scope of PRD 20, a package of its own (`code_search_and_symbols`),
  after the second.
- Changing `GET …/scm/search` or `GET …/scm/file`.
- Finding references, type checking, or any analysis that needs a build.
- Outlining several files in one request.

## Background

The source control API (PRD §6, package 20) reads a workspace's bare
repository: refs, commits, trees, files, diffs, blame and search. All reads
are routes of `internal/scm` declared through one helper, share
`[scm].read_timeout` (default 30 s) and answer `503 scm_timeout` past it,
carry the caching headers of SCM-FR-002, and report to
`parley_scm_requests_total`. `GET …/scm/file` serves a file's bytes at a
ref and refuses a directory and a file over its size limit; `GET …/scm/blame`
calls a file binary when its first 8,000 bytes hold a NUL byte.

agentkit-go's `outline` package returns the declarations of one source
file (kind, name, container, signature, exported, start and end line) from
the file's bytes alone: Go with `go/ast`, and Python, JavaScript,
TypeScript, Rust, Java, Kotlin, C#, Ruby, C, C++, PHP, Scala, Lua and Shell
with tree-sitter grammars. Tree-sitter is C: built without cgo, the package
outlines Go alone and reports every other file as having no backend.

`go.mod` holds agentkit-go at `v0.10.0` since package 57, which links
only its packages without cgo into the `parley` CLI. `parleyd` is built
without cgo today: `containers/hub/Dockerfile` builds both programs with
`CGO_ENABLED=0`. The owner accepted cgo in the hub.

## Requirements

### The route

`GET /workspaces/:slug/scm/outline`

- Query: `path` (required), `ref` (default: the default branch, a fork's
  base branch, as for the file route).
- `200`:
  `{"path", "resolved_sha", "language", "supported", "declarations": [{"kind", "name", "container", "signature", "exported", "start_line", "end_line"}]}`,
  declarations in line order. `kind` is one of `func`, `method`,
  `type`, `class`, `interface`, `enum`, `trait`, `const`, `var`,
  `module`, `macro`.
- A file in a language without a grammar answers `supported: false` with
  no declarations.
- Errors: `404 path_not_found`; `400` when `path` is a directory;
  `413 payload_too_large` for a file over 1 MiB; `422`,
  `error_type: binary_file`; `503 scm_timeout`.
- It needs `git:read` for PATs and read access to the workspace
  (SCM-FR-001), works on any workspace and ref the file route serves, a
  fork's own refs included, and is cached as SCM-FR-002 says.

### CLI

`parley scm outline <slug> <path>` with `--ref`, declared like every
command (ADR 02), prints the route's answer.

### The build

`parleyd` in the hub image is built with cgo, so that it outlines every
language of the list. The `parley` CLI is not affected.

### End-to-end flow

**Orientation without a clone.** An agent calls
`parley scm outline demo internal/api/router.go`, reads which functions
and types the file declares and on which lines, and creates its fork
knowing where to work.

## Design Decisions

1. **Outline needs no index and no setting** (PRD 20, decision 7): it
   parses one file's bytes, so it works at any ref. With no code index in
   this package there is nothing to turn off.
2. **cgo in the hub** (owner's decision, PRD 20, decision 6). Consequence
   stated plainly: `parleyd` needs a C toolchain to build with all
   fifteen languages, and repository content is parsed by C code in the
   hub's process, bounded by the 1 MiB file limit and the read timeout.
   The `parley` CLI is not affected.
3. **Constraint: outlines come from agentkit-go** (`outline`), not from
   new code in Parley (PRD 20, decision 10).
4. **This package is the first scope of PRD 20** (decided with the owner
   when the package was written); the two others are named under
   Non-goals, in the order they are to be written.
5. **Profile `full`**: three requirements, and one flow from the CLI
   through the hub to a repository.
6. **A build of `parleyd` without cgo still serves the route**: it
   outlines Go and answers every other language `supported: false`, as
   agentkit-go does without cgo. It is not what the hub image holds.
7. **`language` is agentkit-go's name of the language** (`Go`,
   `Python`, `C++`, `C#`, `Shell`), from the file's extension, and
   `""` for a file of no known language. `supported` is true exactly
   when a backend outlined the file.
8. **A symbolic link is answered `supported: false`** with `language`
   `""` and no declarations: its content is the path it points at, which
   is not followed, as the file route does not follow it.
9. **A binary file is judged as blame judges it**: a NUL byte in the
   file's first 8,000 bytes. It is `422 binary_file`.
10. **The order of the checks** is the file route's, then the outline's:
    `path`, the ref, the caching answer, the path at the ref
    (`404`, `400`), the size (`413`), the bytes (`422`), and only
    then the parse.
11. **The route reports to `parley_scm_requests_total` under the op
    `outline`**, a new value of that label.

## Dependencies

- Package 20 `scm_read_api` (active, every task done): the read
  conventions, the route helper, the resolver, the file lookup and the
  caching this route uses.
- Package 09 `cli_foundation` and ADR 02: the command declaration.
- Package 57 `cli_mcp_server` (active, every task done): serves the new
  command as the tool `scm_outline` because it is in the `scm` group.
- Package 54 `operations_and_packaging` (active, every task done): the
  hub image this package builds with cgo.
- agentkit-go `outline` (released in `v0.10.0`, which `go.mod` holds).

## Verified External API

`github.com/agent-fox-dev/agentkit-go` v0.10.0, package `outline`, read
with `go doc` and in its source:

- `func Outline(ctx context.Context, abs string, src []byte, opts Options) (File, error)`:
  Go files from `go/ast`, the other languages of the extension table from
  their tree-sitter grammar when built with cgo, anything else as none; a
  cancelled context returns `ctx.Err()`.
- `type File struct { Path string; Lang string; Backend Backend; Decls []Decl }`;
  `Decls` is sorted by `StartLine`, ties by `Name`, and never nil.
- `type Decl struct { Kind Kind; Name, Container, Signature string; Exported bool; StartLine, EndLine int }`;
  `EndLine` is 0 when unknown.
- `type Options struct { Root string; MaxFileBytes int64 }`;
  `MaxFileBytes` 0 means 1 MiB.
- `Backend` is `"go/ast"`, `"tree-sitter"` or `"none"`
  (`BackendGoAST`, `BackendTreeSitter`, `BackendNone`).
- `Kind` is one of `func`, `method`, `type`, `class`, `interface`,
  `enum`, `trait`, `const`, `var`, `module`, `macro`.
- `func LangFor(path string, src []byte) string`: the language's name,
  `""` for none.
- Built with `CGO_ENABLED=0` the package compiles, and its tree-sitter
  backend reports no language.

**Unverified**: what `Outline` returns for a file over `MaxFileBytes`
(the route refuses such a file before calling it), and for a file whose
bytes do not parse in its language.

## Glossary

- **Outline**: the declarations of one source file, in line order.
- **Declaration**: one function, method, type, class, interface, enum,
  trait, constant, variable, module or macro a file declares.
- **Supported**: a file whose language has a backend in this build of the
  hub.
