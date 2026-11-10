---
spec_id: "20"
spec_name: "scm_read_api"
title: "Read a repository without a clone"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "6446b9a095b2920f6ba4d261675890f24328a4554ae686ad552d63153bcdf901"
schema_version: 3
source: "docs/prd/06_source_control_api.md"
---

# Read a repository without a clone

## Intent

A client that wants one file, the history of one path, a diff between two commits or the answer to "would this branch still apply" should get exactly that from the hub with one request and no clone. The read half of the source control API serves refs, commits, trees, files, diffs, comparisons, blame and content search of any workspace the caller may read, names the commit every answer was computed from, never shows a half-finished update, and lets answers addressed by commit id be cached forever.

## Goals

- Ten `GET` routes under `/api/v1/workspaces/:slug/scm` answer refs (list and one), commits (list and one), tree, file, diff, compare, blame and search, each naming the resolved commit ids as full 40-character ids.
- Every read needs read access to the workspace and, for a PAT, `git:read`; it works on `ready` and `archived` workspaces and on task forks, and answers `409` on a workspace that is in exclusive maintenance or `failed`.
- A read addressed only by full commit ids answers `Cache-Control: private, max-age=31536000, immutable`; every read carries an `ETag` and answers `304` to a matching `If-None-Match`.
- A read that runs longer than `[scm].read_timeout` (default 30 s) answers `503 scm_timeout`; a file over `[scm].max_file_size` (default 32 MiB) answers `413`.
- `GET …/scm/compare` reports whether `head` can be rebased onto `base` (`clean` or `conflict`) with a conflict report, without moving any ref.
- The nine read commands `parley scm refs|log|show|ls|cat|diff|compare|blame|search` call those routes.

## Non-goals

- Writing through the API: commit creation, ref create, move and delete, server-side rebase, the `[scm].max_commit_size` limit, the events `parley.scm.commit` and `parley.git.push`, and `parley scm commit|rebase` are package 21 `scm_write_api`.
- The error envelope, check order, list paging rules, request ids and rate limits are package 02 `http_api_conventions`; this package obeys them and does not restate them.
- Authentication and the scope registry are packages 04 `authentication_and_login` and 06 `api_keys_and_access_tokens`; the access rule and anti-enumeration are package 07 `workspace_access_rule`; the binding guard is package 08 `workspace_bound_tokens`.
- The workspace record, its statuses and the state precondition are package 10 `workspace_create_and_clone`; the `protected_branches` setting is package 11 `workspace_read_update_settings`.
- Task forks and what makes a branch a tracking branch are package 15 `task_fork_creation`; the ref view of a fork (own branches plus tracking branches at the parent's heads) is package 15's, the same names and heads package 17 `git_smart_http` advertises; this package reads it through the interface `RefView` it declares in `internal/scm`.
- Which change a branch or commit belongs to is package 22 `change_opening_and_versions`; which session introduced a commit is recorded by packages 18 `git_push_rules`, 21 `scm_write_api` and 25 `optimistic_landing`. This package asks through a lookup that answers "none" until those packages supply the facts.
- The members `landed_changes`, `landed_by` and `change` of the conflict report, and the routes that return a conflicted file, are package 26 `conflict_reports_and_resolution`. Here they are always `[]`, `[]` and `null`. The report is one Go type, `ConflictReport`, declared by this package in `internal/gitrepo/conflictreport.go`; packages 21, 23 and 24 return the same type, and package 26 extends it.
- The Prometheus exposition of `parley_scm_requests_total` and the `/metrics` route are package 51 `audit_query_and_retention`. This package only reports each read to a recorder.
- Loading the configuration file and parsing sizes and durations are package 03 `server_lifecycle_and_configuration`; the CLI's settings, output and exit codes are package 09 `cli_foundation`.
- Not covered by this package: following `next_cursor` in the CLI. `parley scm ls` and `parley scm search` have no `--limit` or `--cursor` flag in the source PRD and print the first page.
- Anything that needs a working tree, such as running builds.

## Background

The repository holds no code yet; every file named here is new. The hub (`parleyd`) keeps each workspace as a bare git repository on disk and runs the `git` command (2.38 or later) as a subprocess through `internal/gitrepo`. The source control API and the git server are two doors to the same repository: what is written through one is visible through the other.

The source is §6 of the Parley PRD (`docs/prd/06_source_control_api.md`), requirements SCM-FR-001 to SCM-FR-009, with the read routes of §6.5, the read commands of §6.6, the read limits of §6.3, the keys `[scm].max_file_size` and `[scm].read_timeout` of §6.8 and the read edge cases of §6.9.

Code lives in `internal/scm/` (handlers, revision resolution, caching headers), `internal/gitrepo/` (the read operations on a repository: resolve, objects, history, diff, trial rebase, blame, grep), `internal/config/` (the two `[scm]` keys), `internal/cli/scm.go` (the commands) and `cmd/parleyd/` (wiring).

## Requirements

All routes are under `/api/v1/workspaces/:slug/scm`. A failure is the error envelope of PLAT-FR-011; "error `xyz`" means that envelope with `error_type` `xyz`.

### Access and workspace state (SCM-FR-001, SCM-FR-004, WS-FR-007, WS-FR-066)

Every read route declares need `read` and scope `git:read` (a PAT with `git:write` passes a read check, by the scope rules). The checks run in the order of PLAT-FR-013, so the caller observes: `401 unauthenticated` without a valid credential; `403 insufficient_scope` for a PAT without the scope, whether or not the workspace exists; `404 not_found` for a workspace that does not exist or that the caller may not read, with the same body in both cases; then validation (`400`); then the state precondition.

Reads are served on a workspace in status `ready` or `archived`, on a task fork, and on a fork whose parent is `archived`. A workspace in `pending` or `cloning`, or with a reclone, archive or delete running, and a fork whose parent is in one of those states, answers `409 workspace_busy`. A `failed` workspace answers `409 workspace_not_ready`.

In a fork, reads see the fork's own branches and the tracking branches at the parent's current heads: after the parent's `main` moves, a read of `main` in the fork resolves to the new head, and the ref object says `tracking: true`.

Reads emit no hub events. Each read is reported once to a request recorder with an `op` (`refs`, `commits`, `tree`, `file`, `diff`, `compare`, `blame`, `search`) and a `result` (`ok` for a status below 400, `error` otherwise).

### Revision parameters and consistency (§6.3, SCM-FR-003, §6.9)

A revision parameter (`ref`, `base`, `head`, and `:sha` of the commit route) accepts a branch name, a tag name, a full ref name (`refs/heads/main`, `refs/parley/changes/12/3`) or a full or abbreviated commit id (4 to 40 hexadecimal characters). An annotated tag resolves to its commit. An unknown or ambiguous revision (a name that is both a branch and a tag, an abbreviated id that matches several commits), one that names no commit, and a revision expression such as `main~2` or `HEAD` answer `404 ref_not_found`. Where `ref` is optional and absent it is the workspace's default branch. Every response names the commit it used as a full 40-character id: `resolved_sha` for a single revision, `base_sha` and `head_sha` for two.

A commit reachable only from a deleted branch is still readable by its full id while the object exists.

Each revision is resolved once per request and the whole response is computed from that commit id. A read that races with a push, a land or a rebuild therefore returns the old or the new commit, never a mixture and never an error caused by the race.

### Caching, timeout and limits (SCM-FR-002, §6.3, §6.8)

A read whose every revision parameter is given as a 40-character commit id answers `Cache-Control: private, max-age=31536000, immutable` and an `ETag`. Every other read (a name, an abbreviated id, an omitted `ref`, and both refs routes) answers `Cache-Control: private, no-cache` and an `ETag` derived from the resolved commit ids. A request whose `If-None-Match` names the current `ETag` answers `304` with no body and the same `ETag` and `Cache-Control`.

A read that has not finished after `[scm].read_timeout` (duration, default `30s`) is stopped, its `git` subprocess is ended, and the answer is `503 scm_timeout`. `[scm].max_file_size` (size, default `32MB`, 32 MiB) is the largest file the file route returns. A value of zero or below for either key stops `parleyd` at start with a message naming the key.

### Refs

`GET …/scm/refs` lists ref objects ordered by name, as a paged list. `prefix` defaults to `refs/heads/`; any other prefix is allowed. `GET …/scm/refs/*name` reads one ref by full name, or by branch name when `name` does not start with `refs/`; a ref that does not exist is `404 ref_not_found`.

Ref object: `name` (full ref name), `kind` (`branch` for `refs/heads/`, `tag` for `refs/tags/`, `hub` for `refs/parley/`), `sha` (the commit the ref points at), `protected` (true for a branch in the workspace's protected branches and for every hub-owned ref), `tracking` (true for a tracking branch of a fork), `change` (`{workspace_slug, number}` when the branch is the source of an open or draft change, else `null`).

### Commits (SCM-FR-007)

`GET …/scm/commits` lists the history of `ref`, newest first, as a paged list that also carries `resolved_sha`. Query: `path` (only commits that changed that path), `follow` (with `path`, follow renames of that single file), `since` and `until` (timestamps), `author` (substring of the author's name or e-mail), `first_parent` (default true), `stats` (adds `stats` to each commit), `limit`, `cursor`. `GET …/scm/commits/:sha` reads one commit by any revision, always with `stats`; `files=true` adds `files`, the file changes against the first parent.

Commit object: `sha`, `parents`, `tree_sha`, `author` and `committer` (`{name, email, time}`), `message` (the full message), `stats` (`{files_changed, insertions, deletions}`), `change` (`{workspace_slug, number, version}` or `null`), `session_id` (string or `null`).

Failures: a boolean parameter that is not `true` or `false`, a `since` or `until` that is not an RFC 3339 timestamp, and `follow=true` without `path` are `400 invalid_request`.

### Tree and file (SCM-FR-005)

`GET …/scm/tree` lists a directory at `ref`: tree entries `{path, name, type, mode, sha, size}` ordered by path, with `resolved_sha` and `tree_sha`. `type` is `blob`, `tree`, `commit` (a submodule) or `symlink`; `size` is present for blobs only. `path` defaults to the root; `recursive=true` lists every entry below it. `limit` defaults to 1,000 and may be 1 to 10,000. Failures: `404 path_not_found` for a path that does not exist; `400 invalid_request` when `path` is a file, and for a path that is not a legal repository path (empty segment, leading `/`, a `.` or `..` segment, anything under `.git`).

`GET …/scm/file` reads one file; `path` is required. By default the answer is the raw bytes with `Content-Type: application/octet-stream`, `X-Parley-Blob-Sha`, `X-Parley-Resolved-Sha` and `X-Parley-File-Mode`; a single `Range` answers `206` with that part. With `Accept: application/json` the answer is `{path, sha, mode, size, encoding, content, resolved_sha}`, where `encoding` is `utf8` when the content is valid UTF-8 and `base64` otherwise. The content of a symlink is its target as text. Failures: `404 path_not_found`; `400 invalid_request` when `path` is missing, is a directory or is a submodule entry; `413 payload_too_large` with the member `size` when the file is larger than `[scm].max_file_size`; `416 range_not_satisfiable` for a range that starts at or after the end of the file.

### Diff and compare (SCM-FR-006)

`GET …/scm/diff` returns the difference between `base` and `head` (both required). With `mode=merge_base` (the default) the left side is the merge base of the two, which shows what `head` adds; with `mode=direct` it is `base` itself. Rename detection is on. Query: `path` (repeatable glob filter), `format` (`json` default, `patch`), `context` (lines, default 3, 0 to 20), `patches` (default true; false returns the file list only).

The JSON answer is `{base_sha, head_sha, merge_base_sha, files, stats, truncated}`. Each file is a file change `{path, old_path, status, insertions, deletions, binary, old_sha, new_sha}` plus `patch` (unified diff text) when patches are on; `status` is `added`, `modified`, `deleted`, `renamed`, `copied` or `type_changed`, and `old_path` is set for renames and copies. At most 5,000 files and 8 MiB of patch text are returned; past either limit `truncated` is true. With `format=patch` the answer is `text/x-diff`.

`GET …/scm/compare` says how `base` and `head` relate: `{base_sha, head_sha, merge_base_sha, ahead, behind, commits, files, mergeable, conflict}`. `commits` holds up to 200 commit objects of `head` that are not in `base`, oldest first; `files` the file changes from the merge base to `head`. `mergeable` is the result of a trial rebase of `head` onto `base`: `clean` with `conflict: null`, or `conflict` with a conflict report (§8.3.3): `target_sha`, `head_sha`, `base_sha`, `commit_sha` (the first commit that did not apply) and `files`, each `{path, kind, base, ours, theirs, landed_by}`. The trial moves no ref.

Failures: a missing `base` or `head`, an unknown `mode` or `format`, a `context` outside 0 to 20 and a `patches` value that is not a boolean are `400 invalid_request`; a merge commit among the commits a compare would replay is `400 merge_commits_unsupported`.

### Blame (SCM-FR-008)

`GET …/scm/blame` (`path` required, optional `start_line`, `end_line`) answers `{resolved_sha, path, ranges}`. Blame returns ranges, not lines: `{start_line, end_line, commit, original_path, original_start_line}`, where `commit` is a commit object without `stats` and carries `change` when known. Failures: `404 path_not_found`; `413 payload_too_large` for a file over 2 MiB; `400 invalid_request` for a binary file, a directory, and a line range that is not inside the file.

### Search (SCM-FR-009)

`GET …/scm/search` matches `q` (required, 1 to 500 characters) against file contents at `ref`: as a literal string, or with `regex=true` as an extended regular expression. `case_sensitive` defaults to true. `path` (repeatable glob) limits the files searched. The answer is a paged list of `{path, line, text}` plus `resolved_sha`, at most 200 matches per page, with `text` cut to 500 characters. Binary files are skipped. Failures: `400 invalid_request` for a `q` outside 1 to 500 characters or a regular expression that does not compile; `503 scm_timeout` past the read timeout.

### CLI (§6.6)

Each command needs the settings of PLAT-FR-090, prints the route's response body as one JSON document and exits by PLAT-FR-093 unless stated.

| Command | Flags | Calls |
|---|---|---|
| `parley scm refs <slug>` | `--prefix`, `--limit`, `--cursor` | `GET …/scm/refs` |
| `parley scm log <slug>` | `--ref`, `--path`, `--follow`, `--since`, `--until`, `--author`, `--stats`, `--limit`, `--cursor` | `GET …/scm/commits` |
| `parley scm show <slug> <rev>` | `--files` | `GET …/scm/commits/:sha` |
| `parley scm ls <slug> [path]` | `--ref`, `--recursive` | `GET …/scm/tree` |
| `parley scm cat <slug> <path>` | `--ref`, `--output <file>` | `GET …/scm/file` |
| `parley scm diff <slug> <base> <head>` | `--mode merge_base\|direct`, `--path` (repeatable), `--patch`, `--name-only` | `GET …/scm/diff` |
| `parley scm compare <slug> <base> <head>` | `--fail-on-conflict` | `GET …/scm/compare` |
| `parley scm blame <slug> <path>` | `--ref`, `--lines <start>:<end>` | `GET …/scm/blame` |
| `parley scm search <slug> <query>` | `--ref`, `--path` (repeatable), `--regex`, `--ignore-case` | `GET …/scm/search` |

`cat` writes the file's raw bytes to stdout, or to `--output`; its stdout is not JSON. `diff --patch` prints unified diff text; `diff --name-only` prints the JSON answer without patch text. `compare --fail-on-conflict` prints the answer and exits 3 when `mergeable` is `conflict`.

### End-to-end flows

1. An agent with a PAT holding `git:read` asks `GET …/scm/file?ref=main&path=README.md`; `parleyd` authenticates it, checks scope and access, resolves `main`, and answers the bytes with `X-Parley-Resolved-Sha`. The agent asks again with that 40-character id as `ref` and receives the same bytes marked immutable; it then asks for the diff between two commit ids and receives the file changes.
2. A review agent runs `parley scm compare demo main feature --fail-on-conflict`; the CLI calls `GET …/scm/compare`; `parleyd` runs a trial rebase that conflicts and answers `mergeable: "conflict"` with the report; the CLI prints the answer and exits 3. No ref of the workspace has moved.

## Design Decisions

1. **Profile: full.** Two end-to-end flows cross the CLI, the router, the access rule and a real repository.
2. **Project decisions relied on:** Go; bare repositories on disk; git operations run the `git` binary (2.38 or later) through `internal/gitrepo`, no git library; HTTP is the standard library `net/http` with its method-and-pattern router; the CLI uses `github.com/spf13/cobra`; tests use the standard `testing` package, table-driven, with `net/http/httptest` and repositories made by `git init` in `t.TempDir()`.
3. **Facts owned by later packages come through narrow lookups.** `internal/scm` asks four questions through interfaces: the ref view of a workspace (`RefView`; package 15 supplies the fork's view, which package 17 advertises), whether a branch of a workspace is protected (the protected-branch test of package 18 over package 11's setting and branch-glob matcher), the change of a source branch or of a commit (package 22), and the session of a commit. Until the owning package supplies an implementation, the defaults answer: the repository's own refs, the default branch only, no change, no session. Why: this package is implemented before those packages and must not import them.
4. **The refs routes are never immutable.** They have no revision parameter, so SCM-FR-002's "every revision parameter is a full id" would hold emptily. Their `ETag` is derived from the names and commit ids on the page.
5. **Reads that are not immutable carry `Cache-Control: private, no-cache`.** The PRD names only the `ETag`; `no-cache` makes a client revalidate with it. `If-None-Match` is honoured on immutable reads as well.
6. **The `ETag` covers the resolved commit ids, the query and the representation** (raw or JSON on the file route, which also answers `Vary: Accept`), so two different answers for one workspace never share a tag.
7. **Refs outside `refs/heads/`, `refs/tags/` and `refs/parley/` are not returned** by the refs routes, because `kind` has no value for them. They stay usable as revisions by full name.
8. **A list cursor pins the resolved commit.** The pages of one commit, tree or search listing come from the commit the first page resolved, even when the ref moves between pages.
9. **Boolean query parameters accept `true` and `false` only;** anything else is `400 invalid_request`.
10. **`since` and `until` compare with the committer time; `author` is a case-insensitive substring match.** `follow=true` without `path` is `400`, because following is defined for a single file. A `path` that never existed gives an empty list, not `404`.
11. **A recursive tree listing contains directories as well as their contents;** `size` is present only on entries of type `blob`.
12. **Illegal repository paths in a read are `400 invalid_request`,** by the path rule of §6.3.
13. **The file route:** a `Range` header with several ranges, and any `Range` with the JSON form, is ignored and the whole file is answered with `200`. A range starting at or after the end of the file is `416` with the new specific type `range_not_satisfiable`. The `413` envelope carries `size` in both forms. A submodule entry is `400`, like a directory. The size limit applies to ranged reads too.
14. **Diff truncation:** files are ordered by path; the list is cut at 5,000 entries; patch text is added file by file until the next patch would pass 8 MiB, and later files appear without `patch`. `stats` always covers the whole diff. With `format=patch` the text is cut at a file boundary and the header `X-Parley-Truncated: true` is set. Binary files have `binary: true`, zero counts and no `patch`.
15. **Two revisions without a common ancestor:** `merge_base_sha` is `null` and the left side in `merge_base` mode is the empty tree, since everything in `head` is added.
16. **Compare:** `files` holds at most 5,000 entries without patch text; commit objects in `commits` carry no `stats`. When `head` is contained in `base`, `mergeable` is `clean`. A merge commit in the range to replay is `400 merge_commits_unsupported`, as the server-side rebase answers (SCM-FR-026), because `mergeable` has only two values. The trial may leave unreferenced objects in the repository; it changes no ref.
17. **Blame:** lines are 1-based and inclusive. `start_line` below 1, above `end_line` or past the last line is `400`; an `end_line` past the last line is cut to it. An empty file answers `ranges: []`. Adjacent lines from the same commit with adjacent original lines form one range. "Binary" is what git treats as binary (a NUL byte in the first 8,000 bytes).
18. **Search:** matches are ordered by path, then line; `limit` defaults to 50 and may be 1 to 200. A regular expression that does not compile is `400 invalid_request`. Globs use the syntax of PLAT-FR-057.
19. **CLI:** `diff --name-only` sends `patches=false` and prints the JSON answer; `--patch` with `--name-only` is a usage error. `blame --lines` needs both numbers. `ls` and `search` print the first page. `cat` writes the output file only after a successful answer.
20. **A non-positive `[scm].read_timeout` or `[scm].max_file_size` stops the hub at start.** The PRD gives no validation; a zero limit would refuse every read.
21. **Revisions are the forms of §6.3 and nothing else.** §2.3 of the overview says a `ref` field accepts "any revision the repository can resolve"; §6.3 lists branch name, tag name, full ref name and commit id. This package follows §6.3: expressions (`main~2`, `HEAD`, `@{1}`) are `404 ref_not_found`, so that no caller string is interpreted by git. Resolution order: a string starting with `refs/` is a full ref name only; otherwise a branch or a tag of that name, where both existing is ambiguous; otherwise a commit id of 4 to 40 hexadecimal characters. The resolver in `internal/scm/revision.go` is the only one: packages 21 (`base_ref`, `ref`, `onto`) and 22 resolve revisions through it.
22. **The recorder sees only requests that reach a read route's handler.** A request refused earlier by the shared checks (`401`, `403`, `404` for the workspace) is not reported.
23. **The request recorder reports `ok` for every status below 400 (a `304` included) and `error` otherwise.** The values `ref_moved` and `refused` of the metric belong to writes.
24. **One conflict report type (with packages 21, 23, 24 and 26).** `ConflictReport` is declared once, in `internal/gitrepo/conflictreport.go`, where the trial rebase produces it. The compare route, the server-side rebase of package 21, the mergeability check of package 23 and the restack of package 24 return it with `landed_changes: []`, every `landed_by: []` and `change: null`; package 26 fills those members on the same type.

## Dependencies

- Package 17 `git_smart_http` (draft): the workspace repository as the git server serves it, including hub-owned refs; a fork's tracking branches reach this package through `RefView`, filled by package 15. Through it: package 10 (workspace record, repository storage, state precondition), 07 (access rule), 06 and 04 (credentials and scopes), 02 (HTTP conventions) and 09 (the CLI foundation the commands are built on).
- External: the `git` command, 2.38 or later, on the hub's `PATH`.

## Verified External API

Read in the local Go module cache (`~/go/pkg/mod`); neither module is in `go.mod` yet, so the versions are the ones read, not ones the project has pinned.

- `github.com/spf13/cobra` v1.10.2, `command.go` and `args.go`: `type Command struct { Use string; Args PositionalArgs; RunE func(cmd *Command, args []string) error; … }`; `func (c *Command) AddCommand(cmds ...*Command)`; `func (c *Command) Flags() *pflag.FlagSet`; `func (c *Command) OutOrStdout() io.Writer`; `func ExactArgs(n int) PositionalArgs`; `func RangeArgs(min int, max int) PositionalArgs`.
- `github.com/spf13/pflag` v1.0.10: `func (f *FlagSet) StringVar(p *string, name string, value string, usage string)`; `BoolVar(p *bool, name string, value bool, usage string)`; `IntVar(p *int, name string, value int, usage string)`; `StringArrayVar(p *[]string, name string, value []string, usage string)`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Tracking branch**: In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token. By default the default branch.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Revision**: A branch name, a tag name, a full ref name or a full or abbreviated commit id that the hub resolves to one commit.
- **Hub-owned ref**: Any ref under `refs/parley/`, written only by the hub.
