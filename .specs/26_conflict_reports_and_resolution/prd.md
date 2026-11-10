---
spec_id: "26"
spec_name: "conflict_reports_and_resolution"
title: "Report conflicts as data and resolve them without a clone"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "628c6e52e80b75dfb44d390b2836e0de276765136d0d659fd76a69b9e25ffedd"
schema_version: 3
source: "docs/prd/08_landing_and_policy.md"
---

# Report conflicts as data and resolve them without a clone

## Intent

When a rebase on the hub cannot be done, an agent gets the reason as data it can act on: which files conflict, what kind of conflict each one is, and which landed changes it collided with. An agent that has no clone can then read a conflicted file with conflict markers and hand its resolution back through the API, so that a conflict never forces anyone to fetch the repository.

## Goals

- One conflict report shape, in one type with one attribution of landed changes, for every place where a rebase can fail on the hub: `target_sha`, `head_sha`, `base_sha`, `commit_sha`, `files`, `landed_changes` and `change`.
- Every conflicted file is reported with one of eight kinds (`content`, `add_add`, `modify_delete`, `delete_modify`, `rename`, `mode`, `binary`, `submodule`) and with its three sides (`base`, `ours`, `theirs`).
- `GET /workspaces/:slug/changes/:number/conflict` answers the change's current conflict report, computed against the present head it would be rebased onto.
- `GET /workspaces/:slug/changes/:number/conflict/file?path=` answers a `content`-conflicted file as text with the markers `<<<<<<< target`, `=======` and `>>>>>>> change`.
- `POST /workspaces/:slug/changes/:number/restack` accepts `resolutions` and uses them wherever a listed path conflicts during the replay; success records a `restack` version authored by the caller.
- `parley change conflict` and `parley change resolve` drive those routes from the shell.

## Non-goals

- The land attempt, the landing algorithm, the `409 land_conflict` answer and the `parley.change.land_fail` event: package 25 optimistic_landing. It is built before this package and produces the report of a failed land itself, in the `ConflictReport` type of `internal/gitrepo/conflictreport.go`, with the attribution of landed changes in `internal/land/conflict.go` and the `change` member of a stack land; this package reuses both and defines no second type.
- The restack operation itself (automatic restack, the manual `POST …/restack` without `resolutions`, `needs_restack`, `409 change_closed`, the `parley.change.restack` event, `parley change restack`): package 24 change_stacks_and_restack. This package adds `resolutions` to its route.
- Computing and storing `mergeable` and the report on the change, the debounce, and the `parley.change.conflict` and `parley.change.ready` events: package 23 change_lifecycle_and_mergeability. This package's first task makes the report it stores pass through the same attribution.
- The comparison route and its trial rebase, and the server-side rebase route with its `409 rebase_conflict`: packages 20 scm_read_api and 21 scm_write_api. They carry a report of this type, `ConflictReport`, which package 20 declares in `internal/gitrepo/conflictreport.go` with `landed_changes` and every `landed_by` empty.
- Recording a version, its ref, and the policy evaluation that follows it: packages 22 change_opening_and_versions and 27 landing_policy.
- The path limit of a path-scoped token at a restack, with or without `resolutions` (LAND-FR-080), and revert with its `409 revert_conflict`: package 29 auto_land_revert_and_path_limits.
- Conflict reports of carry-patch rebuilds and remembered resolutions: packages 34 rebuild_execution and 38 carry_patch_rerere_and_maintenance.
- The binding guard that lets a fork-bound token through to the two read routes: package 08 workspace_bound_tokens. The access rule: package 07 workspace_access_rule.
- The error envelope, the order of checks and the body limit (PLAT-FR-010 to PLAT-FR-019): package 02 http_api_conventions. CLI settings, output, exit codes and path encoding (PLAT-FR-090 to PLAT-FR-095): package 09 cli_foundation.
- A size limit of its own for a conflicted file or a resolution: not covered by this package; the request body limit of package 02 applies to `resolutions`.

## Background

The repository holds no code yet; every file named here is new, except that the restack operation in `internal/change/` and the command file `internal/cli/change.go` are first created by packages 22 to 24 and extended here, and that `internal/gitrepo/` is first created by the packages this one depends on. The source is `docs/prd/08_landing_and_policy.md`: §8.3.3 (the conflict report), LAND-FR-030 to LAND-FR-032, the two `conflict` routes of §8.5, the two CLI entries `parley change conflict` and `parley change resolve` of §8.6, and the part of LAND-FR-003 that names the two routes. The restack route is in `docs/prd/07_changes_and_stacks.md` (CHG-FR-034 and its entry in §7.5). User stories: LAND-US-02 and LAND-US-03.

A rebase on the hub is a replay: the commits of some work that are not in its base are applied one after the other on top of a commit, the *onto* commit. Landing replays onto the target head, a restack onto the head of the change below or onto the target, the server-side rebase and the comparison onto a named ref. When one commit does not apply, the replay stops and nothing is moved.

The code of this package lives in `internal/land/` (the attribution it shares with landing and the two read handlers), `internal/gitrepo/` (the conflict report type and what a failed replay step tells about each conflicted path, both first added by package 20 and completed here, and the marker text), `internal/change/` (resolutions in the restack) and `internal/cli/change.go` (the two commands).

## Requirements

### The conflict report (§8.3.3)

The report is built whenever a replay stops at a commit that does not apply. It has exactly seven members:

| Member | Type | Value |
|---|---|---|
| `target_sha` | string | the commit the work was being replayed onto |
| `head_sha` | string | the head that was being replayed |
| `base_sha` | string | the commit the work was based on before |
| `commit_sha` | string | the first commit of the work that did not apply |
| `files` | object[] | one entry per conflicted path of that commit, ordered by `path` |
| `landed_changes` | object[] | `{number, title, landed_sha, session_id}` for each change that landed between `base_sha` and `target_sha` and touched a conflicted path, ordered by `number`; empty when unknown |
| `change` | int or null | in a stack land, the change that conflicted; null everywhere else |

All commit ids are full 40-character ids. A file entry has `path` (the path in the target), `kind`, `base`, `ours`, `theirs` and `landed_by`. `base`, `ours` and `theirs` are `{sha, mode, path}` of the file in the common ancestor (the parent of `commit_sha`), in the target as replayed so far (`ours`) and in the commit of the change (`theirs`); each is null where the file does not exist on that side. `landed_by` holds the numbers of the landed changes that touched this path, ascending, and is empty when none is known.

The kinds:

| `kind` | When |
|---|---|
| `submodule` | the path is a submodule entry (mode `160000`) on any side |
| `binary` | both sides changed the content of a file that git treats as binary |
| `rename` | one side renamed the file and the other side renamed it to another path, deleted it, or added a file at the new path |
| `add_add` | the file has no common ancestor and both sides added different content at the path; also a path that is a file on one side and a directory on the other |
| `modify_delete` | the target changed the file and the change deleted it |
| `delete_modify` | the target deleted the file and the change changed it |
| `mode` | both sides give the file different modes and the content is the same or merges |
| `content` | both sides changed the text of the file and the changes overlap |

Where several rows apply to one path the first row in this order wins. The report is the same JSON wherever it appears: as the `conflict` member of a change, of a land attempt and of a comparison, and as `details` of the error envelope with `409 rebase_conflict`.

`landed_changes` is known when `base_sha` is an ancestor of `target_sha`: the attribution of `internal/land/conflict.go` (first added by package 25 for landing) then takes the commits between the two, asks the commit mapping of landing which landed change each belongs to, and keeps the changes with a commit that touched a conflicted path. Commits that belong to no change (a direct push, a sync from origin) contribute nothing. When `base_sha` is not an ancestor of `target_sha`, or the mapping cannot be read, `landed_changes` and every `landed_by` are empty and the report is still produced.

Building a report changes nothing: no ref of the repository moves and no working directory is left behind.

### Reading the current report (LAND-FR-030, `GET /workspaces/:slug/changes/:number/conflict`)

The route needs `changes:read` (PATs) and read access to the workspace, which is the change's target workspace. It answers `200` with `{"mergeable": "clean" | "conflict", "conflict": <conflict report> | null}`.

The answer is about the present. The *onto* commit of a change is the current head of the change below it when it has a `depends_on`, and the current head of its target branch otherwise. For a `draft` or `open` change, the stored report is used only when the change's `mergeable` is `conflict` and the stored report's `target_sha` and `head_sha` equal the present onto commit and the change's present head. In every other case (the target moved, a new version arrived, `mergeable` is `unknown` or `clean`) the hub runs a trial replay of the change's own commits onto the onto commit and answers from it: `clean` with `conflict: null`, or `conflict` with a fresh report. The read stores nothing and emits no event.

For a `landed` or `abandoned` change nothing is recomputed: the answer is `conflict` with the stored report when the change holds one, and `clean` with `null` otherwise.

Failures: `403 insufficient_scope` for a PAT without `changes:read`; `404 not_found` for a workspace the caller cannot read; `404`, `error_type: change_not_found`, for an unknown number; `409`, `error_type: conflict`, when the target branch of a `draft` or `open` change no longer exists. A token bound to a fork F reaches the route on F's parent only for a change whose source workspace is F; for any other change it gets `404 change_not_found` (LAND-FR-003).

### Reading a conflicted file (LAND-FR-031, `GET …/conflict/file?path=`)

Same scope, access and fork-token rule as the report route. `path` is required; without it, or empty, the answer is `400 invalid_request`. The hub takes the current report as the report route does. When `path` is a `content` entry of it, the answer is `200` with `Content-Type: text/plain; charset=utf-8` and the merged file: the text both sides agree on, and each overlapping region written as

```
<<<<<<< target
(lines of the target)
=======
(lines of the change)
>>>>>>> change
```

The sides are those of the report entry: `ours` is the target as replayed so far, `theirs` the file in `commit_sha`.

Failures: `404`, `error_type: path_not_found`, when the path is not in the current report (which includes every path of a change that is `clean`); `409`, `error_type: not_text_conflict`, when the entry has any other kind.

### Restack with resolutions (LAND-FR-032, `POST …/changes/:number/restack`)

The restack body takes an optional `resolutions` list. Each entry has a `path` and exactly one of `content` (a UTF-8 string), `content_base64` (standard base64 of the bytes) or `delete: true`.

During the replay of package 24, at each commit that does not apply, the hub looks at the conflicted paths of that commit. When every one of them is listed, it takes the listed result for each path (the given bytes, or no file for `delete`) in place of the conflict, keeps what merged cleanly everywhere else, and goes on with the next commit. A path that conflicts in several commits gets its resolution in each of them. A listed path that does not conflict is left as the replay produced it. A resolved file keeps the mode it has in the change; when the change has no file there, the mode it has in the target; otherwise `100644`.

When a conflicted path is not listed, the restack fails as it does without resolutions: `409 rebase_conflict` with the conflict report in `details`, no branch moves and no version is recorded.

Success answers `200 {"status": "restacked", "change": <change object>}`. The source branch moves by compare-and-swap, and the new version has kind `restack` and is `created_by` the caller, not `system`; it then goes through policy like any other version. A change that is already built on its onto commit answers `200` with `status: "up_to_date"` and the resolutions are not used.

Validation, before anything is replayed, each `400 invalid_request`: `resolutions` is not a list; an entry has no `path`, an empty one, one that starts with `/`, or one with an empty, `.` or `..` segment; an entry has none or more than one of `content`, `content_base64` and `delete`; `delete` is `false`; `content_base64` is not valid base64; two entries name the same path.

### `parley change conflict <slug> <number>` (§8.6)

Calls `GET …/changes/:number/conflict`, prints the response body and exits 0. With `--fail-on-conflict`, a response with `mergeable: "conflict"` is still printed on stdout, `condition_not_met` is reported on stderr and the exit status is 3.

With `--file <path>` it calls `GET …/changes/:number/conflict/file` with the path as the query parameter and writes the response text to stdout byte for byte, not wrapped in JSON. `--file` together with `--fail-on-conflict` is a usage error (exit status 2, no request). A hub error is printed as the envelope on stderr and exits 1.

### `parley change resolve <slug> <number>` (§8.6)

Flags: `--file <repo-path>=<local-file>` (repeatable) and `--delete <repo-path>` (repeatable). The command reads each local file and calls `POST …/changes/:number/restack` with one resolution per flag: `{path, content_base64}` for `--file`, `{path, delete: true}` for `--delete`. It prints the response body and exits 0. A `409 rebase_conflict` is printed as the envelope, with the report, on stderr, and exits 1.

Usage errors, each with exit status 2 and no request: neither flag given; a `--file` value without `=`, or with an empty repository path or an empty local path; a local file that cannot be read; the same repository path named twice across both flags.

### End-to-end flow

An agent's change stops applying because another change landed on the same lines. The agent runs `parley change conflict demo 2` and reads the report; `parleyd` runs the trial replay and names the file as `content` and the landed change as `landed_by`. The agent runs `parley change conflict demo 2 --file app.txt` and gets the file with markers, writes the resolved file locally, and runs `parley change resolve demo 2 --file app.txt=./app.txt`. `parleyd` replays the change with the resolution, moves the source branch and records a `restack` version authored by the agent. `parley change conflict demo 2` now reports `clean`.

## Design Decisions

1. One type, one classification, one attribution. The conflict report type is `ConflictReport` of `internal/gitrepo/conflictreport.go` and the classification of a conflicted path is that of the trial rebase beside it, both declared by the earliest producer (package 20) and used by packages 21, 23, 24 and 25; this package completes them there (the kind order, the file-against-directory case, the ordering of `files`) and declares no second type. The attribution that fills `landed_changes` and `landed_by` is the function package 25 adds in `internal/land/conflict.go`; it reads landed changes through a small lookup. `internal/change/` and `internal/scm/` cannot import `internal/land/`, so each takes the attribution as a function value that `parleyd` hands it, and the producers built before this package (comparison, server-side rebase, mergeability, restack) are switched to call it. The PRD names one shape for all of them and no owner.
2. `ours` is the target and `theirs` is the change, so `modify_delete` means "the target changed it, the change deleted it" and `delete_modify` the reverse. The PRD lists both kinds without saying which side is which; this follows the `base`/`ours`/`theirs` order of §8.3.3.
3. One path gets one kind; the order submodule, binary, rename, add_add, modify_delete or delete_modify, mode, content decides. The PRD gives no precedence; the order goes from what a text resolution can least express to what it can.
4. A file on one side against a directory on the other is reported as `add_add`. The PRD's enum has no kind for it and it is the nearest: both sides put something at the path.
5. `files` is ordered by path and `landed_changes` by number, so that two reports of the same conflict are byte-identical.
6. The report read uses the stored report only when it matches the present pair of commits and recomputes otherwise; it stores nothing and emits nothing. The PRD says "recomputed when stale"; a `GET` without side effects leaves package 23 the only writer of `mergeable`.
7. For a dependent change the onto commit is the head of the change below, as in §7.3.4, not the target head.
8. A `landed` or `abandoned` change answers from what is stored, without recomputing. The PRD names no error for it and there is nothing left to rebase.
9. A `draft` or `open` change whose target branch is gone answers `409`, `error_type: conflict`. Its `mergeable` is `unknown` (§7.10) and the response allows only `clean` or `conflict`; the generic type of PLAT-FR-012 is used because §8 defines no specific one.
10. A missing or empty `path` on the file route is `400 invalid_request`. The PRD marks it required and names no error.
11. A listed resolution is used only where its path conflicts, and at every commit where it does. This is the plain reading of "wherever a listed path conflicts".
12. Resolutions are validated as a whole before the replay, and a duplicate path is refused. The PRD gives the entry shape and no errors; refusing early means a bad list never moves a branch.
13. A resolved file takes its mode from the change, then the target, then `100644`. Resolutions carry no mode in the PRD.
14. `parley change conflict --file` prints the file as raw text. PLAT-FR-092 says stdout carries one JSON document, and §8.6 says the flag prints "that file with conflict markers"; the file itself is what an agent saves and edits.
15. `--file` with `--fail-on-conflict` is a usage error: the file route has no `mergeable` to test.
16. `parley change resolve` always sends `content_base64`, so that any file, text or binary, travels unchanged. A `--file` value is split at its first `=`.
17. Language Go; the server runs the `git` command (2.38 or later) as a subprocess through `internal/gitrepo`, and no git library; tests create repositories with `git init` in `t.TempDir()`; HTTP is `net/http`, the CLI is `github.com/spf13/cobra` (project-wide decisions 1, 3, 4, 6 and 7).

## Dependencies

- Package 25 optimistic_landing (draft): the commit mapping from landed commits to changes and the attribution function of `internal/land/conflict.go`, which `landed_changes` and `landed_by` come from; landing is the first producer that fills them.
- Package 24 change_stacks_and_restack (draft): the restack operation and its route, which this package extends with `resolutions`; through it the change object, versions, the replay and the conflict report type in `internal/gitrepo` (packages 20 and 21), the access rule, the HTTP conventions and the CLI foundation.

## Verified External API

- `github.com/spf13/cobra` and `github.com/spf13/pflag`: `cobra.Command`, `(*Command).Flags`, `(*FlagSet).StringArray`, `(*FlagSet).Bool`, `(*FlagSet).String`. **Unverified**: the repository's `go.mod` pins no version yet.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Stack**: Changes linked by `depends_on`, lowest first.
- **Restack**: The hub rebasing a dependent change onto the new head of the change below it, or onto the target once that change has landed.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Land attempt**: One request to land a change (or a stack), with its outcome.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent.
- **Replay**: In this package, applying the commits of some work that are not in its base, one after the other, on top of an onto commit.
- **Onto commit**: The commit a replay builds on: the target head, or for a dependent change the head of the change below.
