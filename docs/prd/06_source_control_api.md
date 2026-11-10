[Parley PRD](00_overview.md) · §6

## 6. Source control API

### 6.1 Purpose and scope

The source control API lets a client read a repository and write to it without a clone. An agent that wants one file, the history of one path or a diff between two commits asks for exactly that. An agent that has produced three edited files can commit them with one request. Review agents, check runners and revert tooling need no working copy at all.

The API and the git server (§5) are two doors to the same repository. Everything written through one is visible through the other, the same push rules apply to both, and both emit the same events.

In scope: reading refs, commits, trees, files, diffs, comparisons and blame; searching file contents; creating commits from file operations; creating, moving and deleting refs; server-side rebase of a branch. Out of scope: changes and landing (§7, §8), which build on this API; anything that needs a working tree, such as running builds.

### 6.2 User stories

**SCM-US-01** As an Agent, I want to read one file at a ref without cloning, so that small tasks cost one request.

**SCM-US-02** As an Agent, I want to commit a set of file edits through the API and be told if the branch moved under me, so that I never overwrite someone else's work.
- A stale `expected_head_sha` answers `409 ref_moved` with the current head.

**SCM-US-03** As a Review agent, I want the diff and the touched paths of a commit range as JSON, so that I can review without a checkout.

**SCM-US-04** As a Developer, I want path history and blame from the hub, so that tools can answer "who changed this and in which change" directly.
- Commit objects name the change that landed them, when there is one.

**SCM-US-05** As an Agent, I want to rebase my branch onto the latest target on the server and get a conflict report if it fails, so that I can keep a branch current without fetching.

**SCM-US-06** As an Operator, I want read responses addressed by commit id to be cacheable, so that repeated reads are cheap.

### 6.3 Concepts and data model

**Revision parameters.** A parameter named `ref` accepts a branch name, a tag name, a full ref name (`refs/heads/main`, `refs/parley/changes/12/3`) or a full or abbreviated commit id. An ambiguous or unknown revision answers `404`, `error_type: ref_not_found`. Responses always name the resolved commit in `resolved_sha`.

**Ref object.**

| Field | Type | Meaning |
|---|---|---|
| `name` | string | Full ref name, for example `refs/heads/main` |
| `kind` | string | `branch`, `tag` or `hub` |
| `sha` | string | The commit the ref points at |
| `protected` | bool | True when the ref is a protected branch (§8) or hub-owned |
| `tracking` | bool | True for a tracking branch of a fork (§4) |
| `change` | object or null | `{workspace_slug, number}` when the branch is the source of an open or draft change |

**Commit object.**

| Field | Type | Meaning |
|---|---|---|
| `sha` | string | Commit id |
| `parents` | string[] | Parent commit ids |
| `tree_sha` | string | Root tree id |
| `author`, `committer` | object | `{name, email, time}` |
| `message` | string | Full commit message |
| `stats` | object | `{files_changed, insertions, deletions}`; present on single-commit reads and when `stats=true` on lists |
| `change` | object or null | `{workspace_slug, number, version}` when the hub knows the commit as a version head or as a commit landed by a change. `null` otherwise, for example for a commit that reached a protected branch by a direct push (GIT-FR-034) or by sync from origin |
| `session_id` | string or null | The session recorded for the push, API commit or land that introduced the commit |

**Tree entry.** `{path, name, type: "blob"|"tree"|"commit"|"symlink", mode, sha, size}`. `size` is present for blobs only.

**File change** (in diffs and comparisons). `{path, old_path, status: "added"|"modified"|"deleted"|"renamed"|"copied"|"type_changed", insertions, deletions, binary, old_sha, new_sha}`. `old_path` is set for renames and copies.

**File operation** (in commit creation).

| `op` | Fields | Effect |
|---|---|---|
| `put` | `path`; `content` (UTF-8 string) or `content_base64`; optional `mode` (`100644` default, `100755`, `120000`) | Create or replace the file |
| `delete` | `path` | Remove the file. The path must exist |
| `move` | `from`, `to` | Rename. `from` must exist and `to` must not |
| `chmod` | `path`, `mode` | Change the mode only |

Paths are repository-relative, use `/`, and MUST NOT be empty, start with `/`, contain `.` or `..` segments, or name anything under `.git`.

**Limits.**

| Limit | Value | Configuration |
|---|---|---|
| File read size | 32 MiB; larger files answer `413` | `[scm].max_file_size` |
| Commit request body | 32 MiB | `[scm].max_commit_size` |
| Operations per commit | 1,000 | fixed |
| Diff size returned | 5,000 files or 8 MiB of patch text, then `truncated: true` | fixed |
| Blame file size | 2 MiB | fixed |
| Search results | 200 matches per page | fixed |
| Read timeout | 30 s, answered `503`, `error_type: scm_timeout` | `[scm].read_timeout` |

### 6.4 Functional requirements

Reading

- **SCM-FR-001** All read routes need `git:read` for PATs and read access to the workspace. They work on any workspace whose status allows reads (§4), including archived workspaces and forks.
- **SCM-FR-002** A read addressed only by full commit ids (every revision parameter is a 40-character id) answers with `Cache-Control: private, max-age=31536000, immutable` and an `ETag`. Any other read answers with an `ETag` derived from the resolved commit ids and honours `If-None-Match` with `304`.
- **SCM-FR-003** Reads never observe a partial update. A ref read during a land, a push or a rebuild returns either the old or the new commit.
- **SCM-FR-004** In a fork, reads see the fork's own branches and the tracking branches at the parent's current heads.
- **SCM-FR-005** The file route returns raw bytes by default, with `Content-Type: application/octet-stream`, `X-Parley-Blob-Sha`, `X-Parley-Resolved-Sha`, `X-Parley-File-Mode` and support for a single `Range`. With `Accept: application/json` it returns `{path, sha, mode, size, encoding, content}` where `encoding` is `utf8` when the content is valid UTF-8 and `base64` otherwise.
- **SCM-FR-006** Diff and compare use the merge base of `base` and `head` as the left side by default (`mode=merge_base`), which shows what `head` adds. `mode=direct` compares the two commits as they are. Rename detection is on.
- **SCM-FR-007** Commit listing takes an optional `path` and then returns only commits that changed that path, following renames of a single file when `follow=true`.
- **SCM-FR-008** Blame returns ranges, not lines: `{start_line, end_line, commit: <commit object without stats>, original_path, original_start_line}`. Each range's commit carries `change` when known.
- **SCM-FR-009** Search matches a literal string or, with `regex=true`, an extended regular expression against file contents at one ref, optionally limited by `path` globs. It returns `{path, line, text}` per match with `text` cut to 500 characters. Binary files are skipped. A search that exceeds the read timeout answers `503 scm_timeout`.

Writing

- **SCM-FR-020** All write routes need `git:write` for PATs and write access to the workspace. They are refused on an archived workspace (`409`, `error_type: workspace_archived`) and on a fork whose parent is archived (`409`, `error_type: parent_archived`, §4).
- **SCM-FR-021** Every write obeys the push rules of §5 exactly as a git push would: hub-owned refs, protected branches, tracking branches of forks, carry-patch push control and path-scoped tokens. A refusal is `403` with the rule's message from GIT-FR-030 and the `error_type` that this section assigns to the rule (`hub_owned_ref`, `protected_branch`, `default_branch`, `tracking_branch`, `integration_branch`, `patch_branch`, `path_not_allowed`), and for `path_not_allowed` the offending `path`.
- **SCM-FR-028 (direct push through the API).** The owner exemption of GIT-FR-034 applies to this API exactly as to a git push, with the same four conditions. A commit creation on a protected branch, a ref move of a protected branch that is a fast-forward, and a server-side rebase that leaves a protected branch a fast-forward of its old head pass for the workspace's owner and the admin token. A ref create, a backwards or sideways ref move and a ref delete of a protected branch are `403 protected_branch` for every caller. The resulting `parley.git.push` event has `direct: true`, and no change is opened for the branch: a `change` object in a commit request on a protected branch is refused with `400 invalid_request` before anything is written.
- **SCM-FR-022 (commit creation).** `POST …/scm/commits` builds one commit and moves one branch:
  1. Resolve the parent. If the branch exists, the parent is its head and `expected_head_sha` is required and MUST equal it; otherwise `409 ref_moved`. If the branch does not exist, `base_ref` is required and the parent is the commit it resolves to.
  2. Apply the operations in order to the parent's tree. An operation that cannot apply (deleting a missing path, moving onto an existing path, a path used as both file and directory) fails the whole request with `400`, `error_type: invalid_operation`, naming the operation's index.
  3. A commit whose tree equals its parent's tree is refused with `400`, `error_type: empty_commit`, unless `allow_empty` is true.
  4. Create the commit with the given `message`, `author` (default: the caller's name and e-mail) and the hub as committer.
  5. Move the branch from the parent to the new commit with a compare-and-swap. A lost race answers `409 ref_moved` and creates nothing visible.
- **SCM-FR-023** A successful commit has the same consequences as a push of that commit (§5): `parley.git.push` and `parley.scm.commit` are emitted, a new version is recorded when the branch is the source of a change, and a change is opened when the request carries a `change` object or the workspace is a fork with `auto_change` on (§7). Opening needs `changes:write` and passes the validation of CHG-FR-003. With an explicit `change` object, a failed check refuses the whole request with that check's error (`403 insufficient_scope`, or the `400`/`409` of CHG-FR-003) before anything is written. In an `auto_change` fork without a `change` object, the commit is kept, the response has `change: null`, and `change_error: {error_type, message}` says why no change was opened, as the `warning:` line does over git (GIT-FR-043).
- **SCM-FR-024** `session_id`, when given, MUST satisfy the session-linking rule of §14; otherwise `400`, `error_type: unknown_session`, and nothing is written. It is recorded on the commit, the events and any version created.
- **SCM-FR-025 (refs).** Creating a ref needs `name` and `sha` or `ref`. Moving a ref needs `expected_sha` and is a compare-and-swap. Deleting a ref takes an optional `expected_sha`. Only `refs/heads/*` and `refs/tags/*` can be written. Moving a branch backwards or sideways is allowed on unprotected branches, as with a forced push, and refused on a protected branch for every caller (SCM-FR-028). Deleting the default branch is refused (push rule 2a: `403`, `error_type: default_branch`).
- **SCM-FR-026 (server-side rebase).** `POST …/scm/rebase` replays the commits of `branch` that are not in `onto` on top of `onto` and moves `branch` to the result with a compare-and-swap guarded by `expected_head_sha`. Nothing changes on conflict: the answer is `409`, `error_type: rebase_conflict`, with a conflict report (§8.3). A branch already based on `onto` answers `200` with `status: "up_to_date"`. Merge commits in the range are refused (`400`, `error_type: merge_commits_unsupported`). Rebasing the source branch of a change creates a new version of kind `rebase`.
- **SCM-FR-027** Write routes accept `Idempotency-Key` (§3). A replayed commit creation returns the commit made the first time.

### 6.5 REST API

All routes are under `/workspaces/:slug/scm`. The conventions of §3 apply. Common errors: `401`; `403` missing scope; `404 workspace not found`; `404 ref_not_found`; `404 path_not_found`; `409 workspace_busy` during exclusive maintenance; `503 scm_timeout`.

#### `GET /workspaces/:slug/scm/refs`
- **Purpose:** list refs.
- **Query:** `prefix` (default `refs/heads/`; `refs/tags/`, `refs/parley/changes/` and any other prefix are allowed), `limit`, `cursor`.
- **Response:** `200` list of ref objects, ordered by name.

#### `GET /workspaces/:slug/scm/refs/*name`
- **Purpose:** read one ref by full name or by branch name.
- **Response:** `200` ref object. **Errors:** `404 ref_not_found`.

#### `POST /workspaces/:slug/scm/refs`
- **Purpose:** create a branch or a tag.
- **Body:** `{"name": "refs/heads/x" | "x", "sha"?: string, "ref"?: string}`. A bare name is a branch. Exactly one of `sha`, `ref`.
- **Response:** `201` ref object. **Errors:** `400` invalid name; `409`, `error_type: ref_exists`; push-rule refusals (SCM-FR-021).
- **Side effects:** `parley.git.push` with `created: true`.

#### `PATCH /workspaces/:slug/scm/refs/*name`
- **Purpose:** move a branch.
- **Body:** `{"sha": string, "expected_sha": string}`.
- **Response:** `200` ref object. **Errors:** `409 ref_moved` with `{"current_sha"}` in the envelope's `details`; push-rule refusals.
- **Side effects:** `parley.git.push` (`forced: true` when the move is not a fast-forward); a new version when the branch is a change's source.

#### `DELETE /workspaces/:slug/scm/refs/*name`
- **Purpose:** delete a branch or tag.
- **Query:** `expected_sha` (optional).
- **Response:** `204`. **Errors:** `409 ref_moved`; `409`, `error_type: ref_in_use` when the branch is the source of a `draft` or `open` change (abandon the change first; a git push that deletes the branch is accepted and abandons the change, CHG-FR-013); push-rule refusals.
- **Side effects:** `parley.git.push` with `deleted: true`.

#### `GET /workspaces/:slug/scm/commits`
- **Purpose:** history of a ref, optionally of one path.
- **Query:** `ref` (default: the default branch), `path`, `follow` (bool), `since`, `until` (timestamps), `author` (substring of name or e-mail), `first_parent` (bool, default true), `stats` (bool), `limit`, `cursor`.
- **Response:** `200` list of commit objects, newest first, plus `resolved_sha`.

#### `GET /workspaces/:slug/scm/commits/:sha`
- **Purpose:** read one commit. `:sha` accepts any revision.
- **Query:** `files` (bool, default false) adds `files`: the file changes against the first parent.
- **Response:** `200` commit object with `stats`.

#### `POST /workspaces/:slug/scm/commits`
- **Purpose:** create a commit from file operations (SCM-FR-022).
- **Body:**

  ```json
  {
    "branch": "greeting",
    "expected_head_sha": "…",          // required when the branch exists
    "base_ref": "main",                // required when it does not
    "message": "Add greeting endpoint",
    "author": {"name": "…", "email": "…"},
    "operations": [
      {"op": "put", "path": "api/greeting.go", "content": "package api\n…"},
      {"op": "delete", "path": "api/old.go"}
    ],
    "allow_empty": false,
    "session_id": "…",
    "change": {"target_branch": "main", "title": "…", "intent": "…", "draft": false}
  }
  ```
- **Response:** `201 {"commit": <commit object>, "branch": <ref object>, "change": {"workspace_slug","number","version"} | null, "change_error": {"error_type","message"} | null}`. `change_error` is set only when an `auto_change` fork could not open the change (SCM-FR-023).
- **Errors:** `400` validation (`invalid_request`, `invalid_operation`, `empty_commit`); `403` push-rule refusals; `409 ref_moved`; `413`; with a `change` object, also the errors of opening a change (SCM-FR-023).
- **Side effects:** SCM-FR-023.

#### `GET /workspaces/:slug/scm/tree`
- **Purpose:** list a directory.
- **Query:** `ref`, `path` (default: the root), `recursive` (bool, default false), `limit` (default 1,000, max 10,000), `cursor`.
- **Response:** `200` list of tree entries ordered by path, plus `resolved_sha` and `tree_sha`. **Errors:** `404 path_not_found`; `400` when `path` is a file.

#### `GET /workspaces/:slug/scm/file`
- **Purpose:** read one file (SCM-FR-005).
- **Query:** `ref`, `path` (required).
- **Response:** `200` bytes or JSON; `206` for a range. **Errors:** `404 path_not_found`; `400` when `path` is a directory; `413` over the size limit (the JSON form names `size`).

#### `GET /workspaces/:slug/scm/diff`
- **Purpose:** the difference between two revisions.
- **Query:** `base` (required), `head` (required), `mode` (`merge_base` default, `direct`), `path` (repeatable glob filter), `format` (`json` default, `patch`), `context` (lines, default 3, max 20), `patches` (bool, default true; false returns the file list only).
- **Response (`json`):** `200 {"base_sha","head_sha","merge_base_sha","files":[<file change> + "patch"?], "stats", "truncated"}`. **Response (`patch`):** `200 text/x-diff`.

#### `GET /workspaces/:slug/scm/compare`
- **Purpose:** how two revisions relate.
- **Query:** `base`, `head` (both required).
- **Response:** `200 {"base_sha","head_sha","merge_base_sha","ahead","behind","commits":[…up to 200 commit objects of head not in base, oldest first…],"files":[<file change>…],"mergeable":"clean"|"conflict","conflict": <conflict report> | null}`. `mergeable` is the result of a trial rebase of `head` onto `base`.

#### `GET /workspaces/:slug/scm/blame`
- **Purpose:** who last changed each line (SCM-FR-008).
- **Query:** `ref`, `path` (required), `start_line`, `end_line`.
- **Response:** `200 {"resolved_sha","path","ranges":[…]}`. **Errors:** `404 path_not_found`; `413` over the blame limit; `400` for a binary file.

#### `GET /workspaces/:slug/scm/search`
- **Purpose:** search file contents at a ref (SCM-FR-009).
- **Query:** `q` (required, 1–500 characters), `ref`, `path` (repeatable glob), `regex` (bool), `case_sensitive` (bool, default true), `limit`, `cursor`.
- **Response:** `200` list of matches plus `resolved_sha`.

#### `POST /workspaces/:slug/scm/rebase`
- **Purpose:** rebase a branch on the server (SCM-FR-026).
- **Body:** `{"branch": string, "onto": string, "expected_head_sha"?: string, "session_id"?: string}`.
- **Response:** `200 {"status":"rebased"|"up_to_date","branch": <ref object>,"old_head_sha","new_head_sha","onto_sha","change": {...} | null}`.
- **Errors:** `409 ref_moved`; `409 rebase_conflict` with the conflict report in `details`; `400 merge_commits_unsupported`; push-rule refusals.

### 6.6 CLI

The CLI covers the calls a person or a script needs most. The remaining routes (ref create, move and delete) are REST only.

#### `parley scm refs <slug>`
- **Flags:** `--prefix <string>`, `--limit`, `--cursor`. **Calls:** `GET …/scm/refs`.

#### `parley scm log <slug>`
- **Flags:** `--ref`, `--path`, `--follow`, `--since`, `--until`, `--author`, `--stats`, `--limit`, `--cursor`. **Calls:** `GET …/scm/commits`.

#### `parley scm show <slug> <rev>`
- **Flags:** `--files`. **Calls:** `GET …/scm/commits/:sha`.

#### `parley scm ls <slug> [path]`
- **Flags:** `--ref`, `--recursive`. **Calls:** `GET …/scm/tree`.

#### `parley scm cat <slug> <path>`
- **Flags:** `--ref`, `--output <file>` (default: stdout, raw bytes). **Calls:** `GET …/scm/file`. With the default output this command's stdout is raw bytes, not JSON.

#### `parley scm diff <slug> <base> <head>`
- **Flags:** `--mode merge_base|direct`, `--path` (repeatable), `--patch` (print unified diff text instead of JSON), `--name-only`. **Calls:** `GET …/scm/diff`.

#### `parley scm compare <slug> <base> <head>`
- **Calls:** `GET …/scm/compare`. **Flags:** `--fail-on-conflict` (exit 3 when `mergeable` is `conflict`).

#### `parley scm blame <slug> <path>`
- **Flags:** `--ref`, `--lines <start>:<end>`. **Calls:** `GET …/scm/blame`.

#### `parley scm search <slug> <query>`
- **Flags:** `--ref`, `--path` (repeatable), `--regex`, `--ignore-case`. **Calls:** `GET …/scm/search`.

#### `parley scm commit <slug>`
- **Purpose:** commit local files through the API without a clone.
- **Flags:** `--branch <name>` (required), `--base <ref>` (for a new branch), `--expected-head <sha>` (default: the branch's head read just before the commit), `--message <text>` (required), `--put <repo-path>=<local-file>` (repeatable), `--delete <repo-path>` (repeatable), `--move <from>=<to>` (repeatable), `--session <id>`, `--change-target <branch>`, `--title`, `--intent`, `--draft`.
- **Calls:** `GET …/scm/refs/*name` when `--expected-head` is absent, then `POST …/scm/commits`.
- **Exit codes:** `409 ref_moved` exits 1 with the envelope; the command does not retry by itself.

#### `parley scm rebase <slug> <branch>`
- **Flags:** `--onto <ref>` (required), `--expected-head <sha>`. **Calls:** `POST …/scm/rebase`. A conflict prints the envelope with the conflict report and exits 1.

### 6.7 Events

| Event type | When | Metadata |
|---|---|---|
| `parley.scm.commit` | A commit was created through the API | `branch`, `sha`, `parent_sha`, `files` (paths touched, at most 100) |
| `parley.git.push` | Any ref was created, moved or deleted through the API (same event as a git push, §5) | `ref`, `before`, `after`, `created`, `deleted`, `forced`, `direct` (true for a direct push of a protected branch, SCM-FR-028), `via: "api"` |

Reads emit no events. They are counted in `parley_scm_requests_total{op,result}` (§14).

### 6.8 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[scm].max_file_size` | size | `32MB` | Largest file the file route returns |
| `[scm].max_commit_size` | size | `32MB` | Largest commit-creation request body |
| `[scm].read_timeout` | duration | `30s` | Deadline of a read request |

### 6.9 Edge cases and error handling

- A commit to a protected branch with the owner's API key succeeds as a direct push; the same request with the owner's workspace-bound or path-scoped PAT is `403 protected_branch`.
- A `put` whose content equals the existing file is allowed; the commit is refused as empty only when no operation changes the tree.
- Two operations on the same path apply in order, so `put a` then `delete a` leaves no `a`.
- A `put` under a path that is currently a file (`a` is a file, put `a/b`) is `invalid_operation`. A `put` onto a path that is currently a directory is `invalid_operation`.
- Symlink content (`mode: 120000`) is the link target as text. Submodule entries can be read as tree entries of type `commit` and cannot be written.
- Committing to a branch of a fork that is a tracking branch is refused with `tracking_branch`; the response names the fork's base so the client can create its own branch.
- A request that names both `expected_head_sha` and `base_ref` for an existing branch ignores `base_ref`.
- A rebase where every commit of the branch is already contained in `onto` moves the branch to `onto` and reports `rebased` with zero replayed commits.
- Reads at a commit that is reachable only from a deleted branch still succeed by full commit id while the object exists. The hub keeps every commit that a change version or a land attempt refers to, until the workspace is recloned or deleted (§4).
- A workspace in `pending` or `cloning` status answers every source control route with `409 workspace_busy`.
