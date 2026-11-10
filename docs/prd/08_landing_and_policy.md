[Parley PRD](00_overview.md) · §8

## 8. Landing and policy

### 8.1 Purpose and scope

Landing adds a change's commits to its target branch. In Parley it is **optimistic**: the hub rebases the change onto the current head of the target on the server and then moves the target with a compare-and-swap. There is no queue and no per-workspace lock. Two changes that touch different things land at the same time. If the target moved in between, the hub simply tries again on the new head. Only a real conflict stops a land, and it comes back as a structured report that an agent can act on.

Because nothing is serialized, safety comes from two other places. **Policy** decides before landing what each change needs, by the paths it touches: nothing, named checks, a person's approval, or a block. And **after** landing, every land is an event, every direct push of a protected branch by its owner is an event too, every landed commit maps back to its change, and a landed change can be reverted with one call.

In scope: land attempts, the landing algorithm, conflict reports and server-side resolution, protected branches, policy and its evaluation, approvals, checks, automatic landing, revert, path-scoped tokens as they affect landing. Out of scope: the change object (§7); running checks (external runners report them); publishing landed branches to origin (§4).

### 8.2 User stories

**LAND-US-01** As an Agent, I want to land my change with one call and not wait behind unrelated changes.
- Two lands on the same target that do not conflict both succeed, in either order, without either caller retrying.

**LAND-US-02** As an Agent, I want a conflict returned as data: which files, what kind of conflict, and which landed change I collided with, so that I can fix it and try again.
- A conflicting land changes nothing on the target.

**LAND-US-03** As an Agent without a clone, I want to fetch the conflicted file with markers and submit my resolution through the API.

**LAND-US-04** As a Developer, I want to say "docs land by themselves, `billing/**` needs me, everything needs the `test` check", once, per workspace.

**LAND-US-05** As a Developer, I want to approve a version of a change, and have my approval survive a pure rebase but not a content change.

**LAND-US-06** As a Check runner, I want to report `passed` or `failed` for a named check on a version using a token that can do nothing else: a PAT of a user with write access to the workspace that carries only `checks:write`.

**LAND-US-07** As an Agent, I want to set `auto_land` and walk away: the change lands as soon as policy is satisfied.

**LAND-US-08** As a Developer, I want to ask "what would this change need?" before it exists, and I want agents to be able to ask the same.

**LAND-US-09** As a Developer, I want to revert a landed change with one call when something breaks afterwards.

**LAND-US-10** As a Developer, I want to give an agent a token that can only change files under `services/search/**`.

**LAND-US-11** As an Operator, I want a land that was in flight during a restart to finish or fail cleanly afterwards, never to half-apply.

### 8.3 Concepts and data model

#### 8.3.1 Protected branches

- The workspace setting `protected_branches` (§4) is a list of branch globs. Its default is the default branch.
- **Branch globs.** A glob is matched against the whole branch name (without `refs/heads/`). `*` matches any run of characters except `/`. `**` matches any run of characters including `/`. Every other character matches itself, so a glob without wildcards names exactly one branch. A branch is protected when at least one glob matches it. `rules[].targets` (8.3.4) uses the same matching.
- A protected branch moves in exactly three ways: a successful land, a sync from origin (§4), and a **direct push** from the workspace's owner or the admin token.
- **Direct push (owner exemption).** A direct push is a fast-forward update of a protected branch by git push, API commit or API ref move (§5, §6). It is accepted only when the caller is the workspace's owner or the admin token, and the credential is an API key, the admin token, or a PAT that is not bound to a workspace and has no `paths`. A workspace-bound token and a path-scoped token are never exempt, whoever their user is. An org member is never exempt. The rule is the same on both surfaces (GIT-FR-034, SCM-FR-028).
- Forced updates and deletes of a protected branch are refused for every caller, including the owner and the admin token. So is creating a branch that a protected glob matches; such a branch appears by sync from origin, or is created before the glob is set.
- The workspace setting `protect.owner_push` (§4; bool, default `true`) turns the exemption off. With `false`, a protected branch moves only by landing and by sync from origin, for every caller.
- A direct push is not a land. Policy is not evaluated, no approval or check is needed, and no land attempt is recorded. It is recorded by its `parley.git.push` event, which carries `direct: true` and the actor (§14). Its commits belong to no change: commit objects show `change: null` (§6).
- Changes may also target unprotected branches. They land the same way; the branch can additionally be pushed directly.

#### 8.3.2 Land attempt

| Field | Type | Meaning |
|---|---|---|
| `id` | string | UUID |
| `workspace_slug` | string | Target workspace |
| `target_branch` | string | |
| `changes` | int[] | The changes this attempt lands, bottom first. One element unless a stack is landed |
| `status` | enum | `running`, `landed`, `conflict`, `blocked`, `failed` |
| `trigger` | enum | `manual`, `auto_land` |
| `strategy` | enum | `rebase` or `squash` (from the workspace setting `land.strategy`) |
| `attempts` | int | How many times the compare-and-swap was tried |
| `onto_sha` | string or null | The target head the successful (or last) try was built on |
| `landed_sha` | string or null | New target head when `landed` |
| `noop` | bool | True when every commit was already in the target and the target did not move |
| `commits` | object[] | `{change, version_sha, landed_sha}` for each landed commit; with `squash`, one entry per change |
| `conflict` | object or null | Conflict report when `status` is `conflict` |
| `blocked` | object or null | `{change, status, requirements[]}` when `status` is `blocked` |
| `error` | string or null | Reason when `failed` (`contention`, `timeout`, `origin_rejected`, `internal`) |
| `override` | object or null | `{reason}` when policy was overridden |
| `policy_revision` | int | Revision of the policy that was evaluated |
| `requested_by` | object | `{actor_type, actor_id, username}` |
| `session_id` | string or null | |
| `created_at`, `finished_at` | timestamp | |

`landed`, `conflict`, `blocked` and `failed` are final. A new attempt is a new call.

#### 8.3.3 Conflict report

The same shape is used wherever a rebase can fail: landing, restack (§7), server-side rebase (§6), comparison (§6), revert, and carry-patch rebuilds (§10).

| Field | Type | Meaning |
|---|---|---|
| `target_sha` | string | The commit the work was being rebased onto |
| `head_sha` | string | The head that was being rebased |
| `base_sha` | string | The commit the work was previously based on |
| `commit_sha` | string | The first commit of the work that did not apply |
| `files` | object[] | One entry per conflicted file, see below |
| `landed_changes` | object[] | `{number, title, landed_sha, session_id}`: changes landed between `base_sha` and `target_sha` that touched a conflicted file. Empty when unknown |
| `change` | int or null | In a stack land, the change that conflicted |

File entry:

| Field | Type | Meaning |
|---|---|---|
| `path` | string | Path in the target |
| `kind` | enum | `content`, `add_add`, `modify_delete`, `delete_modify`, `rename`, `mode`, `binary`, `submodule` |
| `base`, `ours`, `theirs` | object or null | `{sha, mode, path}` of the file in the common ancestor, in the target (`ours`) and in the change (`theirs`). Null where the file does not exist on that side |
| `landed_by` | int[] | Numbers of the landed changes that touched this path |

For `content` conflicts the hub can produce the file with conflict markers (`GET …/changes/:number/conflict/file`).

#### 8.3.4 Policy

A workspace has one policy document. It is data, set through the API, and versioned by a `revision` that increases on every update.

```json
{
  "revision": 7,
  "default": {"approvals": 0, "checks": ["test"]},
  "rules": [
    {"name": "docs",    "paths": ["docs/**", "**/*.md"], "require": {"approvals": 0, "checks": []}},
    {"name": "billing", "paths": ["billing/**"], "targets": ["main", "release/*"],
     "require": {"approvals": 1, "approvers": ["owner", "user:dana"], "checks": ["test", "billing-e2e"]}},
    {"name": "secrets", "paths": ["deploy/secrets/**"], "require": {"block": true}}
  ]
}
```

| Element | Type | Meaning |
|---|---|---|
| `rules[].name` | string | Unique within the policy, `^[a-z0-9][a-z0-9_-]{0,62}$` |
| `rules[].paths` | string[] | Globs (`*`, `**`, `?`) matched against repository-relative file paths. Required, non-empty |
| `rules[].exclude` | string[] | Globs that take files back out of the rule |
| `rules[].targets` | string[] | Branch globs. Absent means every target branch |
| `require.approvals` | int | 0–10. Number of counted approvals needed |
| `require.approvers` | string[] | Who may give counted approvals: `owner`, `member` (any member of the workspace's organization), `user:<username>`. Default `["owner"]` |
| `require.checks` | string[] | Names of checks that must be `passed` |
| `require.fresh_checks` | bool | Default false. When true, check results reported for an earlier, patch-equivalent version do not count |
| `require.block` | bool | Default false. When true, a change touching a matching file cannot land |
| `default` | requirement | Applies to files that no rule matches. Same fields as `require` |

Limits: at most 200 rules; the document is at most 64 KiB. A workspace that never set a policy has `{"revision": 0, "default": {"approvals": 0, "checks": []}, "rules": []}`: everything may land.

**Evaluation.** For a change:

1. Take every file the current version touches (both paths of a rename).
2. For each file, select the first rule in order whose `targets` match the change's target branch and whose `paths` match the file and whose `exclude` does not. If none, select `default`.
3. Each distinct selected rule (and `default`, if selected) yields one **requirement** on the change: `{rule, paths (the change's files it was selected for, at most 50), approvals: {required, counted, approvers}, checks: [{name, status}], block, satisfied}`.
4. A requirement is satisfied when it does not block, its counted approvals reach `required`, no counted rejection is outstanding, and every named check is `passed`.
5. `landing.status` is:
   - `blocked` when any requirement blocks, or the change cannot land for a structural reason (`reason`: `nothing_to_land`, `target_not_found`, `draft`, `merge_commits`);
   - `pending` when no requirement blocks and at least one is unsatisfied;
   - `ready` when all are satisfied.

Policy is evaluated against the change's own commits only. In a stack, each change is evaluated separately.

#### 8.3.5 Approval

| Field | Type | Meaning |
|---|---|---|
| `id` | string | UUID |
| `change`, `version` | int | The version the verdict is on |
| `verdict` | enum | `approve` or `reject` |
| `comment` | string | Up to 16,000 characters |
| `by` | object | `{actor_type, actor_id, username}` |
| `counts_for` | string[] | Names of the requirements this approval currently counts toward |
| `carried_from` | int or null | The version the verdict was first given on, when it was carried over |
| `created_at` | timestamp | |

- A user has at most one verdict per change version. A new verdict replaces the earlier one.
- **Counting.** An approval counts toward a requirement when its user is among the requirement's approvers and it was **not** given with a credential that created any version of the change, the approved one or an earlier one. Versions created by the hub (`restack`, `sync`) do not reset this. A person approving their agent's work with their own API key counts. An agent approving the version it just pushed does not.
- **Carry-over.** When a new version is patch-equivalent to an approved or rejected version (§7), the verdicts are carried to it. Any other new version starts with none.
- A counted `reject` keeps its requirement unsatisfied until its author replaces or withdraws it, or a version that is not patch-equivalent arrives.

#### 8.3.6 Check

| Field | Type | Meaning |
|---|---|---|
| `name` | string | `^[a-z0-9][a-z0-9_.-]{0,62}$` |
| `change`, `version` | int | |
| `status` | enum | `pending`, `passed`, `failed` |
| `summary` | string | Up to 2,000 characters |
| `url` | string | Optional link to the run |
| `details` | object | Optional free-form JSON, up to 16 KiB |
| `reported_by` | object | |
| `updated_at` | timestamp | |

One result per (version, name); a later report replaces it. A required check with no result counts as `pending`. Results carry over to patch-equivalent versions unless the requirement sets `fresh_checks`.

Checks are reported on a version, not on the rebased result of a land. This is deliberate: verifying every rebased result before it lands would serialize landing. Workspaces that need that guarantee for some paths use `fresh_checks` together with a runner that rebases before testing, and accept the wait.

### 8.4 Functional requirements

Who may land

- **LAND-FR-001** Landing needs `changes:write` (PATs) and one of: write access to the target workspace; or being the change's author while the workspace setting `land.by_author` is true (default). The second case is what lets an org member land their own change from their fork once policy is satisfied (§17.3, decision 11). "Author" means the user behind the credential that opened the change. A fork-bound token lands under whichever case applies to its user; the binding only confines it to changes whose source is its fork (LAND-FR-003).
- **LAND-FR-002** Nobody lands past policy, with one exception: the workspace owner, with an API key or an unbound PAT, or the admin token may pass `override: {"reason": "<text>"}`. A workspace-bound PAT can never override, whoever its user is (`403`, `error_type: override_not_allowed`), so an agent holding a fork or workspace token cannot land past policy. Overrides skip unmet approvals and checks, never `block` rules, are recorded on the land attempt and in `parley.change.land`, and can be disabled per workspace with `land.allow_override: false`.
- **LAND-FR-003 (fork-bound tokens).** A token bound to a fork F (§3) reaches these routes of this section on F's parent, and only for changes whose source workspace is F: `POST …/changes/:number/land` (never with `override`; with `stack: true` only when every change of the stack has source F); `GET …/changes/:number/lands`, and `GET …/lands/:id` for an attempt that lands such a change; `GET …/changes/:number/conflict` and `…/conflict/file`; `GET …/changes/:number/approvals` and `GET …/changes/:number/checks`. It also reaches `GET …/policy` and `POST …/policy/evaluate`. Every other route of this section on the parent answers it `403 token_bound`: the workspace-wide `GET …/lands`, `PUT …/policy`, recording or withdrawing a verdict, reporting a check, and revert. A fork token therefore cannot certify or revert its own work; review agents and check runners use their own tokens.

The landing algorithm

- **LAND-FR-010 (preconditions).** In this order: the change is `open` (`409`, `error_type: change_not_open`); `expected_version`, when given, equals the current version (`409`, `error_type: version_mismatch`); no other land attempt for this change is `running` (`409`, `error_type: land_in_progress`, naming it); every change below it in the stack is landed, unless `stack` is true (`409`, `error_type: depends_on_unlanded`).
- **LAND-FR-011 (policy gate).** Policy is evaluated for the change (each change, for a stack). If any is not `ready` and no valid override is given, the attempt ends `blocked`. The HTTP answer is `409`, `error_type: policy_blocked`, with the land attempt, including its requirements, in `details`.
- **LAND-FR-012 (rebase).** Read the target head *T*. Replay the change's own commits (the commits of the current version that are not in its base) onto *T*:
  - With `rebase`, each commit is replayed in order. Authors and messages are kept. The committer is the hub's landing identity. When the version already sits directly on *T*, the commits are used as they are and nothing is rewritten.
  - With `squash`, one commit is created per change. Its message is the change title, a blank line, the intent, and the trailers `Parley-Change: <slug>#<number>` and, when known, `Parley-Session: <id>`. Its author is the author of the version's head commit; other authors are listed as `Co-authored-by` trailers.
  - Commits whose content is already in *T* are dropped. If nothing is left, the attempt ends `landed` with `noop: true`, `landed_sha` = *T*, and the target is not moved.
  - Merge commits are refused when the change is opened (CHG-FR-003: `400`, `error_type: merge_commits_unsupported`). A later version that brings one in is recorded, and the change's `landing.status` becomes `blocked` with reason `merge_commits` until a version without merge commits arrives. So they do not occur here.
- **LAND-FR-013 (conflict).** If a commit does not apply, the attempt ends `conflict` with a conflict report. The target is untouched. The change stays `open` with `mergeable: conflict`. The HTTP answer is `409`, `error_type: land_conflict`.
- **LAND-FR-014 (compare-and-swap).** Move the target from *T* to the new head only if it still is *T*. If it moved, go back to LAND-FR-012 with the new head. After `[land].max_attempts` tries (default 10) the attempt ends `failed` with `error: "contention"`; the caller may simply try again.
- **LAND-FR-015 (atomic bookkeeping).** The ref move, the change's transition to `landed`, the commit mapping, the land attempt's final status and the `parley.change.land` event are one atomic step. After a crash, the hub is in exactly one of two states: not landed, or landed with all of these recorded.
- **LAND-FR-016 (stack landing).** With `stack: true` the attempt covers the change and every unlanded change below it. All of them are replayed bottom first onto *T* and the target moves once, to the top. It is all or nothing: one conflict or one policy miss stops the whole attempt, and the report names the change.
- **LAND-FR-017 (after landing).** After a successful land, outside the atomic step and each best effort:
  1. delete the source branch when `land.delete_source` is true, its head is still the head of the landed version (a compare-and-swap, so commits pushed later are never discarded) and no other `draft` or `open` change uses it;
  2. restack the change's dependants (§7);
  3. schedule mergeability checks for the other changes on the target (§7);
  4. start the early expiry clock of the source fork when all its changes are landed or abandoned (§4);
  5. in a carry-patch workspace, when the target is a patch branch, trigger a rebuild (§10).
- **LAND-FR-018 (no publication).** Landing changes only the hub's repository. Nothing is pushed to origin or to a forge. Publishing is the explicit branch push of §4. The one exception is a patch branch of a carry-patch workspace whose settings forward or mirror it to origin (CP-FR-086, CP-FR-088); a forward that origin refuses ends the attempt `failed` with `error: "origin_rejected"`.
- **LAND-FR-019 (durability and time).** A land attempt is durable. An attempt that was `running` when the hub stopped is run again from LAND-FR-010 at the next start; LAND-FR-015 makes this safe. One attempt is bounded by `[land].timeout` (default 5 min) and then ends `failed` with `error: "timeout"`.
- **LAND-FR-020 (answering the caller).** The land call waits for the attempt up to `wait` seconds (default 30, max 60). A final attempt is answered `200` when `landed` and with the `409` forms above for `conflict` and `blocked`; `failed` is `503`, `error_type: land_failed`. An attempt still running is answered `202` with the attempt, to be polled.
- **LAND-FR-021 (no waiting on others).** Land attempts for different changes MUST run concurrently, including on the same target branch. The only point of mutual exclusion is the compare-and-swap of LAND-FR-014.

Conflict resolution without a clone

- **LAND-FR-030** `GET …/changes/:number/conflict` returns the change's current conflict report, recomputed against the current target head when the stored one is stale.
- **LAND-FR-031** `GET …/changes/:number/conflict/file?path=` returns a `content`-conflicted file as text with standard conflict markers (`<<<<<<< target`, `=======`, `>>>>>>> change`), or `409`, `error_type: not_text_conflict` for other kinds.
- **LAND-FR-032** `POST …/changes/:number/restack` (§7) accepts `resolutions`: a list of `{path, content | content_base64}` or `{path, delete: true}`. During the replay, wherever a listed path conflicts, the hub takes the given resolution instead of failing. A conflict on a path that is not listed still fails the restack. Success records a new version of kind `restack`, authored by the caller, which then goes through policy like any other version.

Policy

- **LAND-FR-040** Reading the policy needs read access to the workspace and `changes:read`. Replacing it needs `workspaces:write` and owner or admin access. A fork has no policy of its own: changes are governed by the policy of their target workspace.
- **LAND-FR-041** `PUT` replaces the whole document, validates it (names, globs, limits, known approver forms, existing usernames), increases `revision`, and takes an optional `If-Match: <revision>` that fails with `412`, `error_type: revision_mismatch`, when the stored revision differs. The last 50 revisions are kept and readable.
- **LAND-FR-042** After a policy update the hub re-evaluates every `draft` and `open` change of the workspace. Changes whose status changes emit `parley.change.ready` or get their new requirements silently; no approval or check is discarded.
- **LAND-FR-043 (evaluate).** `POST …/policy/evaluate` answers what a set of paths would need, without a change: input `{paths, target_branch?}` or `{change: <number>}`, optionally with a candidate `policy` document to try; output the requirements and the resulting status as they would be with no approvals and no checks.
- **LAND-FR-044** Policy is re-evaluated on: a new version, a retarget, an approval or its withdrawal, a check report, a policy update. The result is stored on the change as `landing`.

Approvals and checks

- **LAND-FR-050** Recording a verdict needs `changes:approve` (PATs) and read access to the target workspace. `changes:approve` is never implied by another scope. Any user who can read the change may record a verdict; whether it counts is decided by §8.3.5. The admin token has no user and is refused (`403 forbidden`); it may still withdraw a verdict and override (LAND-FR-002).
- **LAND-FR-051** A verdict is given on the current version unless `version` names another. A verdict on a version that is neither current nor patch-equivalent to the current one is recorded and has no effect on landing.
- **LAND-FR-052** Reporting a check needs `checks:write` (PATs) and write access to the target workspace: check runners are the owner's automation. A read-only member gets `403 forbidden`. `checks:write` is never implied by another scope. A report on an unknown version is `404`.
- **LAND-FR-053** Verdicts and checks cannot be given on `landed` or `abandoned` changes (`409 change_closed`), and can be given on drafts.

Automatic landing

- **LAND-FR-060** When a change with `auto_land: true` becomes `ready` and `clean`, the hub starts a land attempt with `trigger: "auto_land"`. Permission is checked as the user who set the flag; the attempt's events have actor `system` (§14). That user must be allowed to land at that moment (LAND-FR-001); otherwise the flag is cleared and `parley.change.land_fail` is emitted with reason `not_permitted`.
- **LAND-FR-061** An automatic attempt that ends `conflict` or `blocked` leaves `auto_land` on; the next transition to `ready` and `clean` tries again. One that ends `failed` is retried with backoff (2 s doubling to 5 min) up to 10 times, then `auto_land` is cleared.
- **LAND-FR-062** In a stack, automatic landing proceeds bottom up: a change is landed automatically only when the change below it is landed.

Revert

- **LAND-FR-070** `POST …/changes/:number/revert` on a landed change creates, on the server, a branch `revert/<number>` in the target workspace holding commits that undo the landed commits in reverse order, and opens a change from it with `reverts: <number>`, a generated title (`Revert "<title>"`) and an intent that names the original change. A suffix (`revert/<number>-2`) is used when the branch exists.
- **LAND-FR-071** The reverting change is an ordinary change: policy applies, and `auto_land` may be requested in the same call. If the landed commits can no longer be undone cleanly the answer is `409`, `error_type: revert_conflict`, with a conflict report, and nothing is created.
- **LAND-FR-072** Reverting needs `changes:write` and write access to the target workspace, or authorship of the original change when `land.by_author` is true.

Path-scoped tokens

- **LAND-FR-080** A PAT with `paths` (§3) can create a version only if every file that version changes matches one of its globs. This is enforced where the version is created: at git push (§5), at API commit, ref move and rebase (§6), when a change is opened (§7), and at a restack, with or without `resolutions`. Where the update adds no new commit (a ref move or an open onto existing commits), the files checked are those of the resulting version against its base, so a path-scoped token cannot adopt out-of-path commits made with another credential. This check concerns the credential that creates the version; LAND-FR-081 is the separate check on the credential that lands.
- **LAND-FR-081** A path-scoped token can land and revert changes, and set `auto_land` on them, only when every file of the change matches its globs; otherwise `403 path_not_allowed`. An automatic land started for a flag set with such a token is checked the same way when it starts.
- **LAND-FR-082** Path limits and policy are independent. A token limited to `services/search/**` still needs whatever policy requires for those paths.

### 8.5 REST API

All routes are under `/workspaces/:slug`, where `:slug` is the target workspace. The conventions of §3 apply. Unless a route says otherwise, `GET` routes need `changes:read` and read access to the workspace. `POST …/revert` needs `changes:write` and LAND-FR-072. Recording a verdict and `DELETE …/approvals/:id` need `changes:approve` and read access (LAND-FR-050). Reporting a check needs `checks:write` and write access (LAND-FR-052). Common errors: `401`; `403 insufficient_scope`; `403 forbidden`; `403 token_bound` (LAND-FR-003); `404 workspace not found`; `404 change_not_found`.

#### `POST /workspaces/:slug/changes/:number/land`
- **Purpose:** land a change, or its stack.
- **Auth / scope:** `changes:write`; LAND-FR-001.
- **Body:** `{"expected_version"?: int, "stack"?: bool, "override"?: {"reason": string}, "session_id"?: string}`. **Query:** `wait` (seconds, 0–60, default 30).
- **Response:** `200` land attempt (`landed`); `202` land attempt (`running`).
- **Errors:** `409 change_not_open`, `version_mismatch`, `land_in_progress`, `depends_on_unlanded`, `policy_blocked`, `land_conflict` (each with the land attempt in `details` where one was created); `403`, `error_type: override_not_allowed`; `400 unknown_session` (`session_id` fails the session-linking rule of §14); `503 land_failed`.
- **Side effects:** LAND-FR-015, LAND-FR-017; `parley.change.land` or `parley.change.land_fail`.

#### `GET /workspaces/:slug/changes/:number/lands`
- **Purpose:** list the land attempts of a change, newest first.
- **Auth / scope:** `changes:read`. **Response:** `200` list of land attempts.

#### `GET /workspaces/:slug/lands`
- **Purpose:** list land attempts of the workspace.
- **Query:** `status` (repeatable), `target_branch`, `since`, `limit`, `cursor`. Newest first.
- **Response:** `200` list of land attempts.

#### `GET /workspaces/:slug/lands/:id`
- **Purpose:** read one land attempt (used to poll a `202`).
- **Response:** `200` land attempt. **Errors:** `404`, `error_type: land_not_found`.

#### `GET /workspaces/:slug/changes/:number/conflict`
- **Purpose:** the change's current conflict report (LAND-FR-030).
- **Response:** `200 {"mergeable": "clean" | "conflict", "conflict": <conflict report> | null}`.

#### `GET /workspaces/:slug/changes/:number/conflict/file`
- **Purpose:** one conflicted file with markers (LAND-FR-031).
- **Query:** `path` (required). **Response:** `200 text/plain`. **Errors:** `404 path_not_found` when the path is not conflicted; `409 not_text_conflict`.

#### `GET /workspaces/:slug/policy`
- **Purpose:** read the policy. **Query:** `revision` (default: current).
- **Response:** `200` policy document, with `ETag: "<revision>"`, `updated_at`, `updated_by`.

#### `PUT /workspaces/:slug/policy`
- **Purpose:** replace the policy (LAND-FR-041).
- **Auth / scope:** `workspaces:write`; owner or admin.
- **Body:** `{"default": requirement, "rules": [...]}`.
- **Response:** `200` policy document with the new `revision`.
- **Errors:** `400`, `error_type: invalid_policy`, naming the rule and field; `412 revision_mismatch`; `400` on a fork (`error_type: not_supported_on_fork`).
- **Side effects:** `parley.policy.update`; re-evaluation of open changes.

#### `POST /workspaces/:slug/policy/evaluate`
- **Purpose:** what would these paths, or this change, need (LAND-FR-043).
- **Auth / scope:** `changes:read`.
- **Body:** `{"paths"?: string[], "target_branch"?: string, "change"?: int, "policy"?: <policy document>}`. Exactly one of `paths`, `change`.
- **Response:** `200 {"policy_revision", "status", "requirements": [...]}`.

#### `POST /workspaces/:slug/changes/:number/approvals`
- **Purpose:** approve or reject a version.
- **Auth / scope:** `changes:approve`.
- **Body:** `{"verdict": "approve" | "reject", "version"?: int, "comment"?: string}`.
- **Response:** `201` approval (or `200` when it replaces the caller's earlier verdict on that version).
- **Errors:** `409 change_closed`; `404` unknown version.
- **Side effects:** `parley.change.approve` or `parley.change.reject`; policy re-evaluation.

#### `GET /workspaces/:slug/changes/:number/approvals`
- **Purpose:** list verdicts. **Query:** `version` (default: all). **Response:** `200` list of approvals, newest first.

#### `DELETE /workspaces/:slug/changes/:number/approvals/:id`
- **Purpose:** withdraw a verdict. Only its author, the workspace owner or the admin token may.
- **Response:** `204`. **Side effects:** policy re-evaluation.

#### `PUT /workspaces/:slug/changes/:number/checks/:name`
- **Purpose:** report a check result.
- **Auth / scope:** `checks:write`; write access to the workspace (LAND-FR-052).
- **Body:** `{"status": "pending" | "passed" | "failed", "version"?: int, "summary"?: string, "url"?: string, "details"?: object}`.
- **Response:** `200` check.
- **Side effects:** `parley.change.check`; policy re-evaluation.

#### `GET /workspaces/:slug/changes/:number/checks`
- **Purpose:** list check results. **Query:** `version` (default: current, including carried-over results). **Response:** `200` list of checks, by name.

#### `POST /workspaces/:slug/changes/:number/revert`
- **Purpose:** open a change that reverts a landed change (LAND-FR-070).
- **Body:** `{"title"?: string, "intent"?: string, "draft"?: bool, "auto_land"?: bool, "session_id"?: string}`.
- **Response:** `201` the new change.
- **Errors:** `409`, `error_type: change_not_landed`; `409 revert_conflict`; `409`, `error_type: already_reverted`, naming the open or landed reverting change.
- **Side effects:** a new branch and change; `parley.change.revert` on the original; `parley.change.open` on the new one.

### 8.6 CLI

#### `parley change land <slug> <number>`
- **Flags:** `--stack`, `--expected-version <n>`, `--override <reason>`, `--session <id>`, `--wait` (poll a running attempt to the end), `--timeout`, `--poll-interval`.
- **Calls:** `POST …/changes/:number/land`, then `GET …/lands/:id` with `--wait`.
- **Output:** the land attempt. A conflict or policy block prints the envelope, which contains the attempt with its report or requirements.
- **Exit codes:** 0 `landed`; 1 any other final status, or timeout; 2 usage.

#### `parley change lands <slug> <number>`
- **Calls:** `GET …/changes/:number/lands`.

#### `parley land list <slug>`
- **Flags:** `--status` (repeatable), `--target`, `--since`, `--limit`, `--cursor`. **Calls:** `GET …/lands`.

#### `parley land show <slug> <land-id>`
- **Calls:** `GET …/lands/:id`.

#### `parley change conflict <slug> <number>`
- **Flags:** `--file <path>` (print that file with conflict markers instead of the report), `--fail-on-conflict` (exit 3).
- **Calls:** `GET …/changes/:number/conflict` or `GET …/changes/:number/conflict/file`.

#### `parley change resolve <slug> <number>`
- **Purpose:** restack with resolutions, without a clone.
- **Flags:** `--file <repo-path>=<local-file>` (repeatable), `--delete <repo-path>` (repeatable).
- **Calls:** `POST …/changes/:number/restack` with `resolutions`.

#### `parley change approve <slug> <number>`
- **Flags:** `--version <n>`, `--comment <text>`. **Calls:** `POST …/changes/:number/approvals` with `verdict: "approve"`.

#### `parley change reject <slug> <number>`
- **Flags:** `--version <n>`, `--comment <text>` (required). **Calls:** `POST …/changes/:number/approvals` with `verdict: "reject"`.

#### `parley change approvals <slug> <number>`
- **Flags:** `--version <n>`, `--withdraw <id>`. **Calls:** `GET …/changes/:number/approvals` or `DELETE …/approvals/:id`.

#### `parley change check <slug> <number> <name>`
- **Flags:** `--status pending|passed|failed` (required), `--version <n>`, `--summary`, `--url`. **Calls:** `PUT …/changes/:number/checks/:name`.

#### `parley change checks <slug> <number>`
- **Flags:** `--version <n>`. **Calls:** `GET …/changes/:number/checks`.

#### `parley change revert <slug> <number>`
- **Flags:** `--title`, `--intent`, `--draft`, `--auto-land`, `--session <id>`. **Calls:** `POST …/changes/:number/revert`.

#### `parley policy get <slug>`
- **Flags:** `--revision <n>`. **Calls:** `GET …/policy`.

#### `parley policy set <slug>`
- **Flags:** `--file <path>` (JSON or TOML; `-` for stdin; required), `--if-revision <n>`. **Calls:** `PUT …/policy`.

#### `parley policy evaluate <slug>`
- **Flags:** `--paths <list>` or `--change <number>`; `--target <branch>`; `--policy-file <path>`. **Calls:** `POST …/policy/evaluate`.

### 8.7 Background behaviour

- **Land attempts** run on a bounded pool (`[land].workers`, default 8). The pool limits how many rebases are computed at once; it never orders them. An attempt waiting for a worker is still `running`.
- **Recovery.** At start, every `running` attempt is run again (LAND-FR-019).
- **Automatic landing** reacts to `parley.change.ready` transitions (LAND-FR-060 to LAND-FR-062).
- **Retention.** Land attempts are kept for the life of the workspace. Policy revisions beyond the last 50 are dropped.
- **Metrics.** Every land attempt that reaches a final status is counted in `parley_lands_total{result}` and observed in `parley_land_attempts` and `parley_land_duration_seconds` (§14).

### 8.8 Events

All have `resource_type: "change"` and `resource_id: "<number>"` except `parley.policy.update` (`resource_type: "policy"`, `resource_id: "<slug>"`; the revision is in the metadata).

| Event type | When | Metadata |
|---|---|---|
| `parley.change.approve` | An approving verdict was recorded | `version`, `approval_id`, `counts_for` |
| `parley.change.reject` | A rejecting verdict was recorded | `version`, `approval_id`, `counts_for` |
| `parley.change.check` | A check result was reported | `version`, `name`, `status` |
| `parley.change.land` | A land attempt ended `landed` (one event per landed change) | `land_id`, `target_branch`, `onto_sha`, `landed_sha`, `strategy`, `attempts`, `noop`, `trigger`, `override`, `policy_revision` |
| `parley.change.land_fail` | A land attempt ended `conflict`, `blocked` or `failed` | `land_id`, `status`, `reason`, `files` (for conflicts) |
| `parley.change.revert` | A reverting change was opened for this change | `reverted_by` |
| `parley.policy.update` | The policy was replaced | `revision`, `rules` (count) |

A landed target branch also produces `parley.git.push` for that ref with `via: "land"` (§5), so that consumers following pushes see every movement of a branch in one place.

### 8.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[land].max_attempts` | int | `10` | Compare-and-swap tries per attempt |
| `[land].timeout` | duration | `5m` | Upper bound of one attempt |
| `[land].workers` | int | `8` | Concurrent rebases |
| `[land].committer_name`, `[land].committer_email` | string | `Parley`, `parley@localhost` | Committer of rebased and squashed commits |

Workspace settings (§4): `protected_branches`, `protect.owner_push` (true), `land.strategy` (`rebase`), `land.by_author` (true), `land.delete_source` (true), `land.allow_override` (true).

### 8.10 Edge cases and error handling

- **Two lands race on one target, no conflict.** Both end `landed`. The second has `attempts: 2` and its commits sit on top of the first's.
- **Two lands race and conflict.** One ends `landed`. The other ends `conflict`, and its report's `landed_changes` names the first.
- **The same change is landed twice at once.** The second call gets `409 land_in_progress`. After the first finished, a further call gets `409 change_not_open`.
- **A new version arrives during a land.** The attempt lands the version it started with. A push that arrives after the change is `landed` records no version on it. The branch is no longer the source of an open change, so the push is mapped like any other (GIT-FR-040): in an `auto_change` fork, or with `parley.change`, it opens a new change for the commits the target does not contain; otherwise the branch just moves. Callers that must not land a stale version pass `expected_version`.
- **Policy changes during a land.** The attempt uses the revision read at LAND-FR-011 and records it.
- **Approval withdrawn during a land.** As above: the gate was passed.
- **The target branch is deleted or unprotected mid-attempt.** A deleted target ends the attempt `failed`. Protection is read once at the start.
- **Sync from origin moves a protected branch** (§4). To landing this is just another mover: a running attempt sees the compare-and-swap fail and retries on the new head.
- **A direct push moves a protected branch** (8.3.1). The same holds: a running attempt retries on the new head, and open changes on that target get their mergeability recomputed as after any move of the target (§7).
- **`protect.owner_push` is switched off while a direct push is in flight.** The setting is read once per push or request. A push that read `true` completes.
- **A stack land where a lower change is already landed** skips it.
- **Squash and stacks.** With `squash`, each change of a stack becomes one commit, in order.
- **Revert of a revert** is allowed and restores the original content.
- **A check named in policy that no runner reports** leaves changes `pending` forever. `POST …/policy/evaluate` shows the missing check; the hub does not time checks out.
- **An approver named `user:<username>` who is later deleted** stays in the policy text; the requirement can then only be met by its other approvers. `PUT` validates usernames at write time only.
- **Self-approval through a second token.** The counting rule compares credentials, not users. A person who wants agent work reviewed by someone else names approvers other than the agent's owner in the rule.
- **Landing onto a carry-patch patch branch** follows the extra preconditions of §10 (for example, a workspace whose patch branches live on origin needs `push_patches_to_origin`). A failed precondition ends the attempt `blocked` with that reason.
- **Empty change after rebase** (`noop`) still moves the change to `landed`, with `landed.sha` equal to the unchanged target head.
