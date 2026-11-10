---
spec_id: "21"
spec_name: "scm_write_api"
title: "Write to a repository without a clone"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "528887a6392aa12187a00caffe03c13ceb880f3e0af31ed42a94d37fbacd24ad"
schema_version: 3
source: "docs/prd/06_source_control_api.md"
---

# Write to a repository without a clone

## Intent

An agent or a developer can change a workspace's repository with plain HTTP requests and no working copy: commit a set of file edits in one request, create, move and delete branches and tags, and rebase a branch on the server. Every such write is guarded by a compare-and-swap, so nobody overwrites work they have not seen, and it obeys the same push rules and has the same consequences as a git push, so the REST API and the git server stay two doors to one repository.

## Goals

- `POST /workspaces/:slug/scm/commits` builds one commit from up to 1,000 file operations and moves one branch, or creates it, in one request.
- A write against a head the caller has not seen answers `409 ref_moved` with the current head and changes nothing.
- `POST`, `PATCH` and `DELETE` on `/workspaces/:slug/scm/refs` create, move and delete branches and tags.
- `POST /workspaces/:slug/scm/rebase` replays a branch onto another revision and answers a conflict report when it cannot.
- Every write is refused by the push rules exactly where a git push of the same update would be, with `403` and one `error_type` per rule.
- The workspace's owner and the admin token can move a protected branch forward through the API as a direct push.
- Every accepted write emits `parley.git.push` with `via: "api"`; a commit also emits `parley.scm.commit`.
- `parley scm commit` and `parley scm rebase` drive the two calls from a shell.

## Non-goals

- The read routes and read commands of the source control API, the ref and commit objects, and `[scm].max_file_size` and `[scm].read_timeout`: package 20 scm_read_api.
- The push rules themselves (which update each rule refuses, the owner exemption's four conditions, the path check of a path-scoped token), the `parley.git.push` event's definition and the update of the workspace's `head_sha`: package 18 git_push_rules. This package calls that rule engine (`Evaluate`) and that ref writer (`Apply` with `via: "api"`) in `internal/gitserver/pushrule`, reports the verdict, and has no ref writer of its own.
- Carry-patch push control (integration and patch branches, forwarding to origin): package 37 carry_patch_push_control, reached through the rule engine.
- Opening a change, recording a version and the validation of CHG-FR-003: package 22 change_opening_and_versions. This package declares the interface `ChangeLinker` in `internal/scm/linker.go` (find the open change of a branch, check an opening, open a change) and package 22 registers its implementation. Versions are recorded by the ref-updated stage package 22 registers with the ref writer of package 18; this package only names the version kind (`commit`, `ref`, `rebase`).
- The mapping of a git push to a change and the `parley.*` push options: package 19 git_push_to_change.
- The session-linking rule: package 49 agent_sessions_and_usage, called through the interface `SessionLinker` declared in `internal/scm/session.go`.
- The full conflict report (the `landed_changes` and `landed_by` attribution) and the conflict routes: package 26 conflict_reports_and_resolution. The report type itself is `ConflictReport` of `internal/gitrepo`, declared by package 20.
- Recording a fork's last activity: the ref writer of package 18 reports every accepted API write on a fork to the recorder of package 16; this package makes no call of its own.
- Restack of dependent changes after a new version: package 24 change_stacks_and_restack.
- The error envelope, check order, body rules and `Idempotency-Key` mechanics: package 02 http_api_conventions. The CLI's settings, output and exit codes: package 09 cli_foundation.
- CLI commands for ref create, move and delete: the PRD keeps those REST only.

## Background

The repository holds no code yet; every file named here is new. The source control API is §6 of the product PRD (`docs/prd/06_source_control_api.md`). Package 20 delivers its read half in `internal/scm` and the git wrapper in `internal/gitrepo`; package 18 delivers the push rule engine and the ref writer in `internal/gitserver/pushrule`. This package adds the write half: the handlers in `internal/scm`, the tree-building and rebase operations they need in `internal/gitrepo`, the `[scm].max_commit_size` key in `internal/config`, and two commands in `internal/cli/scm.go`.

Callers are agents (a PAT, often bound to a task fork and limited to paths), developers (an API key), review and revert tooling, and the operator (the admin token). All routes are under `/api/v1/workspaces/:slug/scm`.

## Requirements

### Who may write, and when (SCM-FR-020)

Every write route needs the scope `git:write` when the credential is a PAT (`403 insufficient_scope` otherwise) and write access to the workspace (`404 not_found` for a caller with no access, `403 forbidden` for a caller with read access only). After that the workspace's status decides: `archived` answers `409 workspace_archived`; a fork whose parent is archived answers `409 parent_archived`; `pending`, `cloning` or a running reclone, archive or delete answers `409 workspace_busy`; `failed` answers `409 workspace_not_ready`. A refused request writes nothing and emits nothing. The five write routes are `POST …/scm/commits`, `POST …/scm/refs`, `PATCH …/scm/refs/*name`, `DELETE …/scm/refs/*name` and `POST …/scm/rebase`.

### Commit creation (SCM-FR-022)

`POST …/scm/commits` takes `branch`, `message`, `operations`, and optionally `expected_head_sha`, `base_ref`, `author {name, email}`, `allow_empty`, `session_id` and `change`. The body may be up to `[scm].max_commit_size` (default 32 MiB); a larger body answers `413 payload_too_large`. At most 1,000 operations are accepted.

1. The parent. If the branch exists, the parent is its head and `expected_head_sha` must equal it; a different or absent value answers `409 ref_moved` with `details.current_sha`. `base_ref` is ignored for an existing branch. If the branch does not exist, `base_ref` is required (`400 invalid_request` without it) and the parent is the commit it resolves to (`404 ref_not_found` when it resolves to nothing).
2. The operations are applied in order to the parent's tree (next area).
3. A commit whose tree equals its parent's tree answers `400 empty_commit`, unless `allow_empty` is true.
4. The commit has the given message, the given author or else the caller's name and e-mail, and the hub as committer.
5. The branch moves from the parent to the new commit by compare-and-swap. A lost race answers `409 ref_moved` and leaves nothing visible: no ref moved, no event, no change touched.

The answer is `201 {commit, branch, change, change_error}` with the commit object and ref object of package 20.

### File operations (§6.3, §6.9)

| `op` | Fields | Effect |
|---|---|---|
| `put` | `path`; `content` (UTF-8) or `content_base64`; optional `mode` (`100644` default, `100755`, `120000`) | create or replace the file |
| `delete` | `path` | remove the file; it must exist |
| `move` | `from`, `to` | rename; `from` must exist and `to` must not |
| `chmod` | `path`, `mode` | change the mode only |

A path is repository-relative with `/` separators and must not be empty, start with `/`, contain a `.` or `..` segment, or name anything under `.git`. A malformed operation (unknown `op`, a bad path, both or neither of `content` and `content_base64`, a mode outside the three) answers `400 invalid_request`. An operation that is well formed and cannot apply to the tree as it stands answers `400 invalid_operation` and names the operation's zero-based index: deleting, moving or changing the mode of a missing path; moving onto an existing path; a `put` under a path that is a file; a `put` onto a directory; any write to a submodule entry. Two operations on one path apply in order, so `put a` then `delete a` leaves no `a`. A `put` whose content equals the file is allowed; only a commit that changes nothing is empty. Symlink content is the link target as text.

### Consequences of a commit (SCM-FR-023, SCM-FR-024, SCM-FR-027)

A successful commit has the consequences of a push of that commit: `parley.git.push` (`via: "api"`) and `parley.scm.commit` (`branch`, `sha`, `parent_sha`, `files`: the paths touched, at most 100) are emitted; a version of kind `commit` is recorded, in the transaction of the ref update, when the branch is the source of a `draft` or `open` change; a change is opened when the request carries a `change` object, or when the workspace is a fork with `auto_change` on and the branch has no `draft` or `open` change. With an explicit `change` object, a failed check (`403 insufficient_scope` without `changes:write`, or the `400`/`409` of CHG-FR-003) refuses the whole request and nothing becomes visible. In an `auto_change` fork without a `change` object the commit is kept, `change` is `null` and `change_error {error_type, message}` says why.

`session_id`, on a commit or a rebase, must satisfy the session-linking rule of §14; otherwise `400 unknown_session` and nothing is written. It is recorded on the commit, the events and any version.

The three `POST` write routes take `Idempotency-Key`: a replay answers `200` with the first answer's body and creates nothing.

### Push rules over the API (SCM-FR-021)

Every write is checked by the rule engine of package 18 as the ref update it is. A refusal is `403` with the rule's message from GIT-FR-030 and this `error_type`:

| Rule | `error_type` |
|---|---|
| 1 hub-owned refs (`refs/parley/**`) | `hub_owned_ref` |
| 2 protected branches | `protected_branch` |
| 2a default branch deletion | `default_branch` |
| 3 tracking branches of a fork | `tracking_branch` (with `details.base_ref`, the fork's base branch) |
| 4 carry-patch, integration branch | `integration_branch` |
| 4 carry-patch, patch branch | `patch_branch` |
| 5 path-scoped token | `path_not_allowed` (with the offending `path`) |

A refused write moves no ref, emits no event and touches no change. A rule that cannot be evaluated answers `500 internal`; rules never fail open.

### Direct push through the API (SCM-FR-028)

The owner exemption of GIT-FR-034 applies as over git. A commit on a protected branch, a fast-forward ref move of one, and a rebase that leaves one a fast-forward of its old head pass for the workspace's owner and the admin token, with an API key, the admin token or a PAT that is neither workspace-bound nor path-scoped, while `protect.owner_push` is true. The `parley.git.push` event then has `direct: true`, and no change is opened and no version recorded for the branch. A `change` object in a commit request on a protected branch answers `400 invalid_request` before anything is written. A create, a backwards or sideways move and a delete of a protected branch are `403 protected_branch` for every caller.

### Refs (SCM-FR-025)

- `POST …/scm/refs` with `{name, sha | ref}` creates a branch or a tag and answers `201` with the ref object. A bare name is a branch. Exactly one of `sha`, `ref`. Errors: `400 invalid_request` (bad name, a namespace other than `refs/heads/`, `refs/tags/` or `refs/parley/`, both or neither target); `404 ref_not_found` (target resolves to no commit); `409 ref_exists`. Event: `parley.git.push` with `created: true`.
- `PATCH …/scm/refs/*name` with `{sha, expected_sha}` moves a ref by compare-and-swap and answers `200` with the ref object. Errors: `409 ref_moved` with `details.current_sha`; `404 ref_not_found`. A move that is not a fast-forward is allowed on an unprotected branch and reported with `forced: true`. A branch that is a change's source gets a version of kind `ref`.
- `DELETE …/scm/refs/*name` with optional `expected_sha` answers `204`. Errors: `409 ref_moved`; `409 ref_in_use` when the branch is the source of a `draft` or `open` change; `403 default_branch`; `404 ref_not_found`. Event: `parley.git.push` with `deleted: true`.

### Server-side rebase (SCM-FR-026)

`POST …/scm/rebase` with `{branch, onto, expected_head_sha?, session_id?}` replays the commits of `branch` that are not in `onto` on top of `onto` and moves `branch` by compare-and-swap. The answer is `200 {status, branch, old_head_sha, new_head_sha, onto_sha, change}`. A branch already based on `onto` answers `status: "up_to_date"` and nothing moves. A branch whose every commit is already in `onto` moves to `onto` and reports `rebased`. On conflict nothing changes and the answer is `409 rebase_conflict` with the conflict report in `details`. Merge commits in the range answer `400 merge_commits_unsupported`. A stale `expected_head_sha` or a lost race answers `409 ref_moved`. Rebasing the source branch of a change records a version of kind `rebase`.

### CLI (§6.6)

- `parley scm commit <slug>` with `--branch` (required), `--message` (required), `--base`, `--expected-head`, repeatable `--put <repo-path>=<local-file>`, `--delete <repo-path>` and `--move <from>=<to>`, `--session`, `--change-target`, `--title`, `--intent`, `--draft`. Without `--expected-head` it first reads the branch with `GET …/scm/refs/*name` and uses its head. It prints the hub's answer. `409 ref_moved` prints the envelope on stderr and exits 1; the command does not retry.
- `parley scm rebase <slug> <branch>` with `--onto` (required) and `--expected-head`. A conflict prints the envelope with the conflict report on stderr and exits 1.

### End-to-end flows

1. An agent with a fork-bound PAT posts a commit with three operations to a new branch of its fork, naming `base_ref`. The hub builds the commit, passes the push rules, creates the branch and emits both events. The agent reads the file back through the read API, then posts a second commit with the old head and receives `409 ref_moved` with the current head.
2. A developer runs `parley scm commit` to add a local file to a branch, another commit lands on the target branch, and the developer runs `parley scm rebase --onto`. The hub replays the commit and moves the branch; the command prints `status: "rebased"`.
3. The workspace's owner commits to the protected default branch with an API key and the commit is accepted as a direct push; the same request with the owner's workspace-bound PAT is refused with `403 protected_branch`, an org member's is refused with `403 forbidden`, and the branch does not move.

## Design Decisions

1. Project decisions relied on: Go; bare repositories on disk; git operations run the `git` binary through `internal/gitrepo` (no git library); `net/http` router; `github.com/spf13/cobra` for the CLI; tests use `testing`, `net/http/httptest` and repositories made with `git init` in `t.TempDir()`.
2. A commit's tree is built without a working tree: a temporary index file, `git read-tree`, `git hash-object -w`, `git update-index --cacheinfo`, `git write-tree`, `git commit-tree`. Objects written for a request that is then refused stay unreferenced; "nothing is written" means no ref moved, no event and no change touched.
3. Order of checks inside commit creation, after the route gate: body shape and operation shape (`400 invalid_request`), the `change` object on a protected branch (`400`), `session_id` (`400 unknown_session`), the parent (`409 ref_moved`, `404 ref_not_found`), the operations (`400 invalid_operation`), emptiness (`400 empty_commit`), the push rules (`403`), the explicit change check, the compare-and-swap. SCM-FR-022 fixes the middle of this order; the rest is the simplest order in which each check has its inputs.
4. An existing branch with no `expected_head_sha` answers `409 ref_moved` with the current head, the literal reading of SCM-FR-022 step 1, so a client can retry from one answer.
5. `operations` may be empty or absent; the commit is then empty and needs `allow_empty`.
6. `invalid_operation` names the index as `details.index`, zero-based. `tracking_branch` names the fork's base as `details.base_ref` (`fork_point.ref`). `path_not_allowed` carries `path` as a named member of `error`, as SCM-FR-021 says.
7. A ref name under `refs/parley/` is refused by rule 1 (`403 hub_owned_ref`); any other namespace outside `refs/heads/` and `refs/tags/` is `400 invalid_request`. This keeps both SCM-FR-021 and SCM-FR-025 true.
8. Tags written through the API are lightweight and can be moved and deleted with the same routes as branches. A ref's target must be a commit.
9. A `PATCH` whose `sha` equals the ref's current commit answers `200` and emits nothing, as a push with no change does (GIT-FR-025).
10. `Idempotency-Key` replays apply to the three `POST` routes, as PLAT-FR-015 defines. `PATCH` and `DELETE` accept the header and ignore it; `expected_sha` already makes them safe to retry.
11. Changes and sessions are reached through two interfaces declared in `internal/scm`, `ChangeLinker` (`linker.go`) and `SessionLinker` (`session.go`), because packages 22 and 49 are built after this one. Until package 22 registers its implementation, no branch is a change's source and a request with a `change` object answers `400 invalid_request`. Until package 49 registers its implementation, every `session_id` is refused with `unknown_session`: an unverifiable reference is never accepted. `SessionLinker` has the same name and check in `internal/gitserver` (package 19) and `internal/change` (package 22). Here and in package 19 it is check-only: this package records no session link, because a commit, ref move or rebase becomes a session link only through the change version that package 22 records for it; package 22's `SessionLinker` alone has a record operation (with packages 19, 22 and 49).
12. The rebase keeps each commit's author and sets the hub as committer. Without `expected_head_sha` the head read at the start of the request guards the compare-and-swap.
13. "Already based on `onto`" means `onto` is an ancestor of, or equal to, the branch head. "Every commit already contained in `onto`" means the replay produces no commit; the branch moves to `onto`.
14. The conflict report of a rebase carries `target_sha`, `head_sha`, `base_sha`, `commit_sha` and `files` with `path`, `kind`, `base`, `ours`, `theirs`; `landed_changes` and each `landed_by` are empty arrays and `change` is null, which §8.3.3 allows when unknown. Package 26 adds the attribution.
15. `parley scm commit` sends operations in command-line order across `--put`, `--delete` and `--move`, sends file content as `content_base64` with no `mode`, and sends a `change` object when any of `--change-target`, `--title`, `--intent`, `--draft` is given. A local file that cannot be read is a `usage_error` (exit 2) and no request is sent.
16. The PRD sets no time limit on writes and none is added; the read timeout of package 20 does not apply to write routes.
17. A path with a `.git` segment at any depth is refused, not only one that starts with `.git/`; git itself refuses such tree entries. A `chmod` takes `100644` or `100755` only: a mode change cannot turn a file into a symlink.
18. An org member has read access only (§3.3.6), so a member's write is refused by the route gate with `403 forbidden` and never reaches the push rules. The callers who reach rule 2 without the exemption are the owner's workspace-bound and path-scoped PATs, and every caller while `protect.owner_push` is false.
19. The hub's committer identity is the one `internal/gitrepo` uses for every commit the hub makes; this package adds no setting for it. When the admin token commits without an `author`, the author is that identity too, because the admin token has no user.
20. Every write goes through `Evaluate` and `Apply` of package 18 (decision 2 of that package). The version of a moved source branch is recorded by the ref-updated stage of package 22 inside `Apply`'s commit step, in one store transaction with the ref update (CHG-FR-010); this package passes the version kind, the session and the caller's credential and reads the recorded version from `Apply`'s result for the `change` member. Opening a change (`ChangeLinker`) happens after the ref has moved, as the mapping stage of package 19 does for a push. A stage error fails the request before the ref moves.
21. Revisions (`base_ref`, the `ref` of a ref creation, `onto`) are resolved with the resolver of package 20, in the four forms of §6.3: branch name, tag name, full ref name, commit id. `sha`, `expected_sha` and `expected_head_sha` are full 40-character ids.
22. The post-push listeners are notified by package 18, not here (with packages 18 and 37). Every write of this package reaches the ref writer with `via: "api"` and the request's session id, and the event step of `Apply` calls the registered post-push listeners with the slug, the accepted updates, that `via` and that session id. This package holds no listener registry and calls no listener, so the carry-patch triggers of package 37 reach API writes through package 18's one registration.
23. Ancestry and single-ref compare-and-swap are package 12's `IsAncestor` and `CompareAndSwapRef` in `internal/gitrepo` (with packages 12 and 18). This package declares neither again: the rebase asks `IsAncestor` whether `onto` is contained in the head, and every ref it moves goes through `Apply` of package 18.
24. Wiring is in `internal/hub/scm.go`: what this package has to be handed (the engine and ref writer of package 18, the event writer, the registration calls for `ChangeLinker` and `SessionLinker`) is composed there.
25. **Writes are counted through package 20's request recorder (with packages 20 and 51).** Each request that reaches a write route's handler is reported once to the request recorder of `internal/scm`, which package 20 declares and package 51 connects to `parley_scm_requests_total{op,result}`. The `op` values are `commit`, `ref_create`, `ref_move`, `ref_delete` and `rebase`. The `result` values are package 20's `ok` (a status below 400) and `error` (any other), plus the two that package 20's decision 23 leaves to writes: `ref_moved` for a `409 ref_moved` and `refused` for a `403` of a push rule. As with reads, a request refused by the shared checks or the write gate before the handler runs is not reported. This package declares no recorder of its own.

## Dependencies

- Package 20 scm_read_api (draft): the `internal/scm` route group, revision resolution, the ref and commit objects, the `[scm]` configuration section, `internal/gitrepo` including the trial rebase this package's rebase extends, and `internal/cli/scm.go`.
- Package 18 git_push_rules (draft): the rule engine that accepts or refuses a ref update and the ref writer with the compare-and-swap of refs, the ref-updated stages, the emission of `parley.git.push` and the call of the post-push listeners, both in `internal/gitserver/pushrule`, and the protected-branch test. Through it: package 12 workspace_sync_from_origin, which declares `IsAncestor` and `CompareAndSwapRef` in `internal/gitrepo`.
- Package 09 cli_foundation (draft) for the two commands and package 52 durable_event_log_and_stream (draft) for `parley.scm.commit`.
- Through interfaces, not dependencies: package 22 (`ChangeLinker`), package 49 (`SessionLinker`).
- The `git` command, 2.38 or later, at run time.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Tracking branch**: In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token. Nobody can force-push or delete it. By default the default branch.
- **Direct push**: A fast-forward update of a protected branch by the workspace's owner or the admin token, over git or the source control API, without a change. Allowed while the workspace setting `protect.owner_push` is true.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version.
- **Conflict report**: The structured description of why a rebase could not be done: files, kinds, and the landed changes involved.
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
