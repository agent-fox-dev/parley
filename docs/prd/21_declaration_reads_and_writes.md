# Read and replace one declaration through the source control API

## Intent

An agent that works without a checkout can read one declaration of a file
and change it by name, instead of fetching and sending back the whole file.
The hub finds the declaration, splices the new text in, refuses a result
that no longer parses, and records an ordinary git commit, so everything
that follows a commit today (versions, changes, policy, events, stock git)
is unchanged.

## Goals

1. `GET …/scm/declaration` returns the text of the declarations of one file
   that match a name, at any ref, with no index involved.
2. `POST …/scm/commits` accepts operations that address a declaration by
   name: `replace_declaration`, `delete_declaration` and
   `insert_declaration`.
3. A commit made with such an operation is a commit like any other: the
   same push rules, the same events, the same version and change
   consequences as a `put` of the resulting file.
4. A declaration operation whose resulting file has a syntax error is
   refused and nothing is written.
5. An address that matches no declaration, or more than one, is refused
   with an answer that lets the caller correct it in one further request.
6. The read is a `parley scm` command, and the operations are flags of
   `parley scm commit`; both are tools of `parley mcp` without further
   work.

## Non-goals

- A new storage format, or any commit that stock git cannot read. The
  overview's non-goal "Replacing git" stands.
- Addressing syntax nodes below a declaration (a statement, an expression,
  a parameter). The unit is the declaration the outline route reports.
- Line-range or patch operations in `POST …/scm/commits`.
- Editing a declaration's leading comment through its address. The
  declaration is the lines the outline reports, which for some languages
  leave the comment out; a caller that changes both uses `put`.
- Formatting, linting, type checking or any check that needs a build. The
  only check is that the file parses.
- Stable identities for declarations across renames.
- Declarations touched by a change, and what awareness and landing do with
  them. It is PRD 22.
- The same operations among a sandbox's tools (PRD 18). A sandbox edits
  files on its own checkout.

## Background

`GET …/scm/outline` (PRD 20, package 58, `internal/scm/outline.go`) returns
the declarations of one file at any ref: `kind`, `name`, `container`,
`signature`, `exported`, `start_line`, `end_line`. It parses the file's
bytes with agentkit-go's `outline` package (go/ast for Go, tree-sitter for
fourteen other languages when the hub is built with cgo), takes files up to
1 MiB, refuses binary files, answers `supported: false` for a language
without a grammar, and runs at most four parses at once.

`POST …/scm/commits` (PRD §6, SCM-FR-022, package 21,
`internal/scm/commit.go`) builds one commit from the operations `put`,
`delete` and `move`, each on a whole file, guarded by `expected_head_sha`.
`parley scm commit` sends them with `--put`, `--delete` and `--move`.

So an agent without a checkout that changes one function of a 2,000-line
file reads the outline, fetches the file, and sends the whole file back in
a `put`. It pays for the file twice in tokens, and nothing stops it from
sending a file that does not parse.

## Requirements

### Reading declarations

`GET /workspaces/:slug/scm/declaration` needs `git:read` for PATs and read
access to the workspace, as every SCM read (SCM-FR-001), and is cached as
SCM-FR-002 says.

- Query: `path` (required), `name` (required, 1 to 200 characters, matched
  exactly and with case), `container` (optional, exact), `kind` (optional,
  one of the outline's kinds), `ref` (default: the default branch).
- `200`: `{"path", "resolved_sha", "blob_sha", "language", "declarations": [{"kind", "name", "container", "signature", "exported", "start_line", "end_line", "content"}]}`.
  `declarations` holds every declaration of the file that matches, in line
  order; `content` is the file's lines `start_line` to `end_line`, with
  their line endings as stored.
- No match: `404`, `error_type: declaration_not_found`.
- A file in a language this hub cannot outline: `422`,
  `error_type: unsupported_language`, with `language` (empty when unknown).
- As the outline route: `404 path_not_found`; `400` when `path` is a
  directory; `413 payload_too_large` over 1 MiB; `422 binary_file`;
  `503 scm_timeout`.
- It works on any workspace and ref the file route serves, a fork's refs
  included, and needs neither `[code_index]` nor the `code_index` setting.

### Declaration operations in a commit

`POST …/scm/commits` accepts, next to `put`, `delete` and `move`:

- `{"op": "replace_declaration", "path", "name", "container"?, "kind"?, "content"}`:
  the file's lines from the declaration's `start_line` to its `end_line`
  are replaced by `content`. A `content` that does not end in a line ending
  gets the file's own.
- `{"op": "delete_declaration", "path", "name", "container"?, "kind"?}`:
  those lines are removed.
- `{"op": "insert_declaration", "path", "content", "after"?: {name, container?, kind?}, "before"?: {…}}`:
  `content` is inserted on the line after the named declaration's last
  line, or before its first, as given and with no blank line added. Exactly
  one of `after` and `before` is required; neither or both is
  `400 invalid_request`. A file with no declaration to name is written
  with `put`.

For each such operation, in the order given and on the file as the
operations before it left it:

1. The address is resolved in the file. It must match exactly one
   declaration. None: `404`, `error_type: declaration_not_found`. More than
   one: `409`, `error_type: ambiguous_declaration`, with `candidates`, each
   `{kind, name, container, signature, start_line, end_line}`, at most 20.
   Both name the operation's index.
2. The file before the operation must parse without a syntax error;
   otherwise `422`, `error_type: unparseable_file`, with `path` and the
   first error's `line`. An address in a file that does not parse cannot be
   trusted.
3. The file after the operation must parse without a syntax error;
   otherwise `422`, `error_type: syntax_error`, with the operation's index,
   `path`, and `errors`, each `{line, column}` in the resulting file, at
   most 20.
4. The result does not have to contain a declaration of the same name: a
   replacement may rename it, split it in two or leave something else.

Other failures: `422 unsupported_language`, `422 binary_file`,
`413 payload_too_large` when the file is over 1 MiB before or after the
operation, `503 scm_timeout` when the parses outlast `[scm].read_timeout`.
A missing `path` is the existing `400 invalid_operation`.

Any refusal fails the whole request and writes nothing, as for the existing
operations. Everything else about the request is SCM-FR-020 to SCM-FR-024
unchanged: the push rules, path-scoped tokens (checked against the paths
the commit changes), `empty_commit` when a replacement equals what it
replaced, the compare-and-swap, the events and the change consequences. The
response is the route's existing response. The commit and its events do not
say which kind of operation made them.

At most 100 declaration operations in one request; one more is
`400 invalid_request`.

### CLI

- `parley scm declaration <slug> <path> <name>` with `--container`,
  `--kind`, `--ref`; prints the route's answer.
- `parley scm commit` gains, each repeatable:
  `--replace-declaration <repo-path>#<name>=<local-file>`,
  `--delete-declaration <repo-path>#<name>`,
  `--insert-after <repo-path>#<name>=<local-file>` and
  `--insert-before <repo-path>#<name>=<local-file>`. The address is split
  at the last `#`; a container is written `<container>.<name>` and split
  at the last dot. The flags carry no `kind`: an address that is still
  ambiguous with its container is sent through the REST route.

### End-to-end flows

1. **Change one function without a clone.** An agent runs
   `parley scm outline demo api/greeting.go`, then
   `parley scm declaration demo api/greeting.go Greet`, then
   `parley scm commit demo --branch greeting --message "…" --replace-declaration api/greeting.go#Greet=greet.go`.
   The branch moves, `parley.git.push` and `parley.scm.commit` are emitted,
   and the change on that branch has a new version.
2. **A replacement that does not parse.** The same commit with a missing
   brace answers `422 syntax_error` with the line; the branch has not
   moved.
3. **Two methods of one name.** `--replace-declaration svc.go#Close=…` on a
   file with `(*Reader).Close` and `(*Writer).Close` answers
   `409 ambiguous_declaration` with both candidates; the agent repeats it
   with `Writer.Close`.

## Design Decisions

1. **A declaration is addressed by `name`, with `container` and `kind` to
   tell apart what shares a name; no line numbers in the address.** Lines
   go stale with every commit; `expected_head_sha` already pins the file.
2. **The read returns every match, the write demands exactly one.** A read
   of two overloads is useful; a write to "one of them" is not.
3. **A file that does not parse before the operation is refused**, not
   edited on a best guess. `put` remains for repairing it.
4. **Syntax is the only check.** It is what the hub can decide from the
   file's bytes alone, as the outline does.
5. **The leading comment is outside the address** in this PRD, because the
   outline's ranges leave it out for some languages.
6. **No new event and no mark on the commit**: consumers of commits should
   not care how the tree was produced.
7. **Replace, delete and insert** (owner's decision). Without insert,
   adding a function would still mean sending the whole file.
8. **The syntax verdict comes from agentkit-go, which gains it** (owner's
   decision). Consequence stated plainly: this cannot be built before
   that addition exists upstream.
9. **Constraint (from PRD 20): declarations come from agentkit-go's
   `outline`**, so the read, the address and the outline route can never
   disagree about where a declaration is.

## Dependencies

- Package 58 `scm_file_outline` (active, all tasks done): the declarations
  and their limits.
- Package 21 `scm_write_api` (active, all tasks done): the commit route
  this extends. Its intent is unchanged; this adds operations.
- Package 20 `scm_read_api` (active, all tasks done): read conventions.
- Package 57 `cli_mcp_server` (active, all tasks done): serves the new command as a tool.
- agentkit-go `outline` (built) and an addition to it that is **not
  built**: a report of the syntax errors of a file's bytes, for the
  languages it outlines. Goal 4 and the two parse rules need it; the read
  route does not.

## Verified External API

`github.com/agent-fox-dev/agentkit-go` v0.10.0, read in the module cache
with `go doc`:

- `outline.Outline(ctx context.Context, abs string, src []byte, opts outline.Options) (outline.File, error)`;
  `File` has `Path`, `Lang`, `Backend`, `Decls`; `Decl` has `Kind`, `Name`,
  `Container`, `Signature`, `Exported`, `StartLine` (1-based, inclusive),
  `EndLine` (1-based, inclusive; 0 when unknown).
- `outline.BackendNone` marks a file with no backend.

**NOT FOUND**: any report of syntax errors. `outline` builds its
declarations from a partial tree when the file has errors (`goast.go`:
"Even if err != nil (syntax errors), we use the partial AST") and exports
neither the errors nor the tree. Assumed of the addition: given a path and
a file's bytes it says whether a backend exists for the language and, if
so, the position (line and column) of every syntax error, for Go without
cgo and for the other fourteen languages with it.

**Unverified**: for which languages a declaration's `StartLine` leaves out
its leading comment; that `EndLine` is never 0 for the fifteen languages.
A declaration whose `EndLine` is 0 cannot be addressed for a write
(`422 unsupported_language`).

## Glossary

- **Declaration**: one entry of a file's outline (PRD 20): a function,
  method, type, constant and so on, with its kind, name, container and
  line range.
- **Address**: `path`, `name`, and optionally `container` and `kind`,
  naming a declaration in a file at one commit.
- **Declaration operation**: an operation of a commit request that changes
  a file through an address instead of replacing the whole file.
