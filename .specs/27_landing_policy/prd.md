---
spec_id: "27"
spec_name: "landing_policy"
title: "Decide what may land by path"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "de0414cfdee131bb27c9b83550f0f691006e73f9c4b9945708d7248a4726a6b2"
schema_version: 3
source: "docs/prd/08_landing_and_policy.md"
---

# Decide what may land by path

## Intent

Landing in Parley is not serialized, so safety has to be decided before a change lands. A workspace owner says once, as data, what each part of the repository needs: nothing, named checks, a person's approval, or a block. Parley turns that policy and the files a change touches into the change's requirements and one landing status, keeps that status current whenever its inputs change, and answers the same question for paths that are not a change yet, so that people and agents know what a piece of work will need before they do it.

## Goals

- One policy document per workspace, set and read through the REST API and the CLI, with a `revision` that increases by one on every replacement and the last 50 revisions readable.
- A document that breaks a rule of the format is refused with `400 invalid_policy`, naming the rule and the field, and nothing is stored.
- For any change, an evaluation that selects one rule (or `default`) per touched file, yields one requirement per distinct selection and one status: `blocked`, `pending` or `ready`.
- The evaluation of a change is stored on the change as `landing` and is evaluated again on a new version, a retarget, an approval or its withdrawal, a check report and a policy update.
- After a policy update every `draft` and `open` change of the workspace carries a `landing` computed against the new revision when the `PUT` has answered `200`.
- `POST …/policy/evaluate` answers what a list of paths, or an existing change, would need with no approvals and no checks, against the stored policy or a candidate document, and writes nothing.
- A workspace that never set a policy lets every change land: revision 0, no rules, a default that requires nothing.
- `parley policy get`, `parley policy set` and `parley policy evaluate` reach the three routes.

## Non-goals

- Recording, replacing, withdrawing, counting and carrying over approvals; reporting and carrying over check results; their routes, events and CLI commands (§8.3.5, §8.3.6, LAND-FR-050 to LAND-FR-053): package 28 `approvals_and_checks`. This package reads approvals and checks through an evidence source that returns none until package 28 supplies the real one.
- The land attempt, the policy gate inside landing (LAND-FR-011), overrides and their refusal (LAND-FR-002), the landing routes: packages 25 `optimistic_landing` and 29 `auto_land_revert_and_path_limits`. Package 25 declares the policy gate in `internal/land`; this package registers its evaluation as that gate. The override rule beside the gate is package 29's.
- Automatic landing in reaction to `parley.change.ready`, revert, and path-scoped tokens at landing (LAND-FR-060 to LAND-FR-082): package 29 `auto_land_revert_and_path_limits`.
- The change object, its versions, the list of files a version touches, and deriving the structural reasons `nothing_to_land`, `target_not_found` and `merge_commits`: packages 22 `change_opening_and_versions` and 23 `change_lifecycle_and_mergeability`. This package reads them from the change.
- Mergeability and the `parley.change.ready` event: package 23 `change_lifecycle_and_mergeability`. `parley.change.ready` has one writer, the landable announcement of package 23 (`internal/change/landable.go`): it writes the event when a change is `open`, its `landing.status` is `ready` and its `mergeable` is `clean`, for a head it has not been announced for since it last was not landable, and forgets the announcement when the change stops being landable. This package writes no event of that type itself; it runs that announcement in every transaction in which it stores a `landing`.
- Protected branches, the workspace settings `protected_branches`, `protect.owner_push` and the `land.*` keys: packages 11 `workspace_read_update_settings` and 18 `git_push_rules`.
- The error envelope, the order of checks, body limits and idempotency (PLAT-FR-010 to PLAT-FR-019): package 02 `http_api_conventions`. The access decision: package 07 `workspace_access_rule`. The binding guard that lets a fork-bound token reach `GET …/policy` and `POST …/policy/evaluate` on the fork's parent and refuses `PUT …/policy` with `403 token_bound`: package 08 `workspace_bound_tokens`.
- CLI settings, output, exit codes and help (PLAT-FR-090 to PLAT-FR-095): package 09 `cli_foundation`.
- Storing events and their visibility: package 52 `durable_event_log_and_stream`. This package hands its events to the event component.
- The path globs of path-scoped tokens (package 06 `api_keys_and_access_tokens`): the `paths` of a token are matched by package 06's matcher in `internal/identity`, in which `?` is an ordinary character. The `paths` and `exclude` of a policy rule also know `?`, so this package has its own path matcher in `internal/policy/glob.go`, used for policy only.
- Branch globs, their validity and their matching: package 11 `workspace_read_update_settings` holds the one branch-glob matcher, `internal/workspace/branchglob.go` (`ValidateBranchGlob`, `MatchBranchGlob`, `MatchAnyBranchGlob`), which `protected_branches` uses too. This package uses it for `targets` and writes none.
- Ownership files in the repository as a source of rules, and timing checks out: not part of the product (§17, §8.10).

## Background

The repository holds no code yet; every file named here is new, except files that earlier packages create and this package extends: `cmd/parleyd/main.go` (package 01) and the change records of `internal/change/` (package 22). The source is `docs/prd/08_landing_and_policy.md`: §8.3.4, LAND-FR-040 to LAND-FR-044, the three policy routes of §8.5, the three `parley policy` commands of §8.6, the retention line of §8.7, the `parley.policy.update` row of §8.8 and the policy lines of §8.10. User stories LAND-US-04 and LAND-US-08.

The code lives in `internal/policy/`: `document.go` (the document, its validation and normal form), `glob.go` (policy path globs), `evaluate.go` (rule selection, requirements, status), `store.go` (revisions), `ports.go` (the narrow interfaces to changes, evidence, users, workspaces and events), `reevaluate.go` (the stored `landing` and its triggers), `handlers.go` (the three routes). The CLI commands are in `internal/cli/policy.go`. End-to-end tests are in `test/e2e/policy_test.go`.

Package 22 gives a change its `landing` member, `{status, reason, requirements[]}`, and package 25 treats every change as ready until this package exists. This package replaces that baseline. Package 28 is built after this one, so the approvals and checks this package reads come through an interface whose first implementation returns no approvals and no check results.

## Requirements

### The policy document (§8.3.4)

A workspace has one policy document: a `default` requirement and an ordered list `rules`.

| Element | Type | Rule |
|---|---|---|
| `rules[].name` | string | required; `^[a-z0-9][a-z0-9_-]{0,62}$`; unique within the document; not `default` |
| `rules[].paths` | string[] | required, at least one entry; each a path glob |
| `rules[].exclude` | string[] | optional; each a path glob; normal form `[]` |
| `rules[].targets` | string[] | optional; when present at least one entry, each a branch glob; absent means every target branch |
| `rules[].require` | requirement | required |
| `default` | requirement | optional; absent means `{"approvals": 0, "checks": []}` |

A requirement:

| Member | Type | Rule |
|---|---|---|
| `approvals` | int | 0 to 10; normal form 0 when absent |
| `approvers` | string[] | when present at least one entry, each `owner`, `member` or `user:<username>` where a user with that username exists when the document is written; normal form `["owner"]` when absent |
| `checks` | string[] | each `^[a-z0-9][a-z0-9_.-]{0,62}$`; normal form `[]` when absent |
| `fresh_checks` | bool | normal form `false` when absent |
| `block` | bool | normal form `false` when absent |

A path glob is 1 to 255 characters and does not start with `/`. A branch glob is 1 to 255 characters and is a valid branch name (WS-FR-011: no control character, space, `~`, `^`, `:`, `?`, `[` or `\`; no `..`, `//` or `@{`; not `@`; does not start with `-` or `/`; does not end with `/`, `.` or `.lock`; no path component that starts with `.` or ends with `.lock`) in which `*` and `**` may also appear.

Limits: at most 200 rules; the document, as the JSON text that was sent, is at most 64 KiB (65 536 bytes). 200 rules and 65 536 bytes are accepted; 201 rules and 65 537 bytes are refused.

A document that breaks any rule above, or has a member of the wrong JSON type, is refused with `400`, `error_type` `invalid_policy`. The error carries `details`: `{"rule": <the rule's name, "default", or null>, "index": <the rule's zero-based position, or null>, "field": <the member, such as "require.approvals" or "paths[1]">}`. The first violation is reported, in this order: size, the top-level shape, the rule count, `default`, then the rules in order, each member in the order of the tables above; a repeated name is reported on the later rule. A text over the size limit, and a candidate document that is not a JSON object, are reported with rule null, index null and field `document`; too many rules with field `rules`. Unknown members are ignored and not stored (PLAT-FR-017).

The stored and returned document is the normal form: every requirement carries all five members, every rule carries `exclude`, and `targets` is present only where it was given.

A workspace that never set a policy has `{"revision": 0, "default": {"approvals": 0, "approvers": ["owner"], "checks": [], "fresh_checks": false, "block": false}, "rules": []}`: everything may land. A task fork has no policy of its own; it always has this document.

### Reading the policy (LAND-FR-040, `GET /workspaces/:slug/policy`)

Needs `changes:read` (PATs) and read access to the workspace. Answers `200` with the document in normal form plus `revision`, `updated_at` and `updated_by` (`{actor_type, actor_id, username}` of the caller that wrote the revision; both members are `null` at revision 0), and the header `ETag: "<revision>"`.

The query parameter `revision` selects an earlier revision. The 50 highest revisions are kept and readable. A revision that was never written or is no longer kept answers `404 not_found`; so does `revision=0` once a policy was set. A value that is not a non-negative integer answers `400 invalid_request`.

A PAT without `changes:read` gets `403 insufficient_scope`; a caller with no access gets `404 not_found` (`workspace not found`). An org member may read. A token bound to a fork may read the policy of the fork's parent (LAND-FR-003). On a fork's own slug the answer is the revision-0 document. An archived workspace can still be read.

### Replacing the policy (LAND-FR-040, LAND-FR-041, `PUT /workspaces/:slug/policy`)

Needs `workspaces:write` (PATs) and owner or admin access. The body is `{"default": requirement, "rules": [...]}`. The hub validates the document, stores its normal form under the next revision (current + 1), and answers `200` with the document as `GET` returns it, including the `ETag` header. In the same transaction it records `parley.policy.update` (`resource_type` `policy`, `resource_id` the slug, action `update`, actor the caller, metadata `revision` and `rules`, the number of rules), drops revisions beyond the 50 highest, and re-evaluates the workspace's changes (below).

An optional header `If-Match: <revision>`, with or without double quotes, makes the write conditional: when the stored revision differs, the answer is `412`, `error_type` `revision_mismatch`, and nothing changes. An `If-Match` value that is not a non-negative integer is `400 invalid_request`.

Refusals after the access decision, in this order, each leaving the stored policy, its revision and the event log unchanged:

1. the workspace is a task fork: `400 not_supported_on_fork`;
2. the body is not a JSON object, or `If-Match` is malformed: `400 invalid_request`; the document is invalid: `400 invalid_policy`;
3. the workspace is archived: `409 workspace_archived`;
4. `If-Match` does not equal the stored revision: `412 revision_mismatch`.

Before that: a PAT without `workspaces:write` gets `403 insufficient_scope`; an org member gets `403 forbidden`; a token bound to a fork gets `403 token_bound` on the parent. A workspace whose status is `pending`, `cloning` or `failed` accepts a policy.

Two replacements that run at the same time without `If-Match` both succeed, with consecutive revisions. Of two that carry the same `If-Match`, exactly one succeeds and the other gets `412`.

### Selecting a rule for a file (§8.3.4, evaluation steps 1 and 2)

Path globs are matched against the whole repository-relative path: `*` matches any run of characters, possibly empty, that contains no `/`; `**` matches any run, possibly empty, including `/`; `?` matches exactly one character other than `/`; every other character matches itself. So `docs/**` matches `docs/a/b.md`, `*.md` matches `README.md` and not `docs/a.md`, and `**/*.md` matches `docs/a.md` and not `README.md`.

Branch globs are matched against the whole branch name without `refs/heads/`, with `*` and `**` as above (§8.3.1); `?` cannot occur. Validity and matching of a `targets` entry are those of the branch-glob matcher of package 11 (`internal/workspace/branchglob.go`).

The files of a change are every file its current version touches against its base, with both the old and the new path of a rename. For each file the selected rule is the first rule, in document order, whose `targets` is absent or has a glob matching the change's target branch, and whose `paths` has a glob matching the file, and whose `exclude` has none matching it. If no rule is selected, `default` is. Policy is evaluated against the change's own commits only; in a stack each change is evaluated separately.

### Requirements and landing status (§8.3.4, evaluation steps 3 to 5)

Each distinct selected rule, and `default` if selected, yields one requirement:

```json
{"rule": "billing", "paths": ["billing/a.go"], "approvals": {"required": 1, "counted": 0, "approvers": ["owner", "user:dana"]},
 "checks": [{"name": "test", "status": "pending"}], "block": false, "satisfied": false}
```

`rule` is the rule's name, or `default`. `paths` holds the change's files the rule was selected for, in ascending byte order, at most the first 50. Requirements are listed in document order of their rules, `default` last. `checks` lists each named check once, in the order of the rule, with status `passed`, `failed` or `pending`; a named check with no result is `pending`.

The counted approvals, whether a counted rejection is outstanding, and the check statuses come from the evidence source, asked per requirement with the change, its current version, the requirement's `approvers`, its check names and its `fresh_checks`. Until package 28 supplies it, the evidence source answers 0 counted approvals, no rejection and no result for every check.

A requirement is satisfied when `block` is false, `counted` is at least `required`, no counted rejection is outstanding and every named check is `passed`.

The result is `{status, reason, requirements}`:

- `blocked` when a requirement blocks (`reason` null), or when the change cannot land for a structural reason, in which case `reason` is that reason: `draft` when the change's state is `draft`, otherwise the reason the change record carries (`nothing_to_land`, `target_not_found`, `merge_commits`). A structural reason wins over everything else; the requirements are still listed;
- `pending` when nothing blocks and at least one requirement is unsatisfied;
- `ready` when all are satisfied, which includes a change with no requirement at all.

### The stored `landing` and its triggers (LAND-FR-044)

The package offers one operation, "re-evaluate change N of workspace W". It reads the current policy revision of the change's target workspace, evaluates the change's current version, stores the result as the change's `landing` and returns the result together with the policy revision it used. The packages that own the triggers call it: a new version and opening (package 22), a retarget and a change of `draft`/`open` (package 23), an approval, its withdrawal and a check report (package 28), and the policy gate of a land (the gate this package registers with `internal/land` of package 25, which records the revision). `GET …/changes/:number` then shows the stored `landing`.

In the transaction that stores the new `landing` the operation runs the landable announcement of package 23. `parley.change.ready` has one writer, the landable announcement of package 23 (`internal/change/landable.go`): it writes the event when a change is `open`, its `landing.status` is `ready` and its `mergeable` is `clean`, for a head it has not been announced for since it last was not landable, and forgets the announcement when the change stops being landable. So a re-evaluation that moves the stored status to `ready` on an `open`, `clean` change yields exactly one `parley.change.ready` (`resource_type` `change`, `resource_id` the number, actor `system`, metadata `number`, `ref`, `version`, `head_sha`), a second re-evaluation that finds it still `ready` yields none, and a move away from `ready` yields none and lets the next move to `ready` be announced again. This package writes no event for a change's `landing` by itself.

A change that is `landed` or `abandoned` is not evaluated: the operation leaves its `landing` as it is and reports that the change is closed. A number that names no change of the workspace is reported as not found.

### Re-evaluation after a policy update (LAND-FR-042)

A successful `PUT` re-evaluates every `draft` and `open` change whose target workspace is this workspace, in the transaction that stores the document. When the `PUT` has answered `200`, each of them carries a `landing` computed against the new revision. A change that becomes ready this way, and is `open` and `clean`, gets `parley.change.ready` through the same announcement; every other change gets its new requirements silently. No approval and no check result is discarded or changed. `landed` and `abandoned` changes are not touched. If the re-evaluation of any change fails, the `PUT` answers `500 internal` and neither the document nor any `landing` has changed.

A single-change re-evaluation that runs at the same time as a policy update never leaves a `landing` computed against the older revision: after both have finished, the stored `landing` is the one for the newer revision.

### Evaluating without a change (LAND-FR-043, `POST /workspaces/:slug/policy/evaluate`)

Needs `changes:read` (PATs) and read access. The body is `{"paths"?: string[], "target_branch"?: string, "change"?: int, "policy"?: <policy document>}` with exactly one of `paths` and `change`. The answer is `200 {"policy_revision", "status", "requirements"}`: the requirements and the status as they would be with no approvals and no check results, so `counted` is 0 and every check is `pending`. `policy_revision` is the workspace's stored revision. Structural reasons are not applied. Nothing is written: no revision, no event, no change's `landing`.

- With `paths`: 1 to 2 000 entries, each a non-empty string that does not start with `/`; repeated entries count once. `target_branch` defaults to the workspace's default branch and need not exist.
- With `change`: the files of that change's current version and its target branch are used, in whatever state the change is. Approvals and checks it already has are ignored.
- With `policy`: the candidate is validated like a `PUT` body and used in place of the stored document.

Errors: `400 invalid_request` when both or neither of `paths` and `change` are given, when `paths` breaks its rule, when `target_branch` is given together with `change`, is empty or is longer than 255 characters, or when `paths` is given without `target_branch` on a workspace that has no default branch yet; `400 invalid_policy` for an invalid candidate, with the same `details` as `PUT`; `404`, `error_type` `change_not_found`, for a number that names no change of the workspace, and for a token bound to a fork when the change's source is not that fork. A token bound to a fork may call the route on the fork's parent (LAND-FR-003). On a fork's own slug the stored document is the revision-0 document. An archived workspace can be evaluated.

### CLI (§8.6)

Each command prints the route's response body on stdout and follows the global behaviour of package 09.

- `parley policy get <slug>` with `--revision <n>`: calls `GET …/policy`, with `?revision=<n>` when the flag is given.
- `parley policy set <slug>` with `--file <path>` (required; `-` reads standard input) and `--if-revision <n>`: reads the document, sends it as JSON with `PUT …/policy`, and sends `If-Match: "<n>"` when `--if-revision` is given. A file whose name ends in `.toml` is read as TOML; anything else, including standard input, is read as JSON and, when it is not valid JSON, as TOML. In TOML the document is the table `default` and the array of tables `rules`.
- `parley policy evaluate <slug>` with `--paths <list>` (comma-separated, repeatable) or `--change <number>`, `--target <branch>` and `--policy-file <path>` (same formats as `--file`): calls `POST …/policy/evaluate` with `paths` or `change`, `target_branch` and `policy`.

A usage error (exit status 2, `usage_error` on stderr, nothing on stdout, no request): a missing slug or `--file`; both or neither of `--paths` and `--change`; `--revision`, `--if-revision` or `--change` that is not a non-negative integer; a file that cannot be read or is neither JSON nor TOML.

### End-to-end flows

1. **Set a policy and see a change follow.** A developer runs `parley policy set api --file policy.json`. The CLI sends `PUT /api/v1/workspaces/api/policy`. `parleyd` validates the document, stores revision 1, records `parley.policy.update` and re-evaluates the open changes of `api`. An agent reads its open change that touches `billing/a.go` and sees `landing.status` `pending` with a requirement of rule `billing`; a change that touches only `docs/` stays `ready`; a change that touches `deploy/secrets/` is `blocked`.
2. **Ask before the work exists.** An agent holding a token bound to its fork runs `parley policy evaluate api --paths billing/a.go,docs/x.md`. `parleyd` answers the two requirements and status `pending`, and writes nothing.

## Design Decisions

1. **Stack.** Go; SQLite through `internal/store` for revisions; `net/http` routes registered with `internal/server`; cobra for the CLI; tests with `testing`, table-driven, `net/http/httptest`, repositories from `git init` in `t.TempDir()` (project decisions 1 to 4, 6 and 7).
2. **Glob dialect and the three matchers.** Policy `paths` and `exclude` are matched by this package's matcher: `*`, `**` and `?` as §8.3.1 defines `*` and `**`, on the whole path, with empty runs allowed, so `**/*.md` does not match a file in the repository root. Token `paths` are matched by package 06's matcher (`?` literal) and never by this one; `targets` by package 11's branch-glob matcher. Why: it is the only definition of matching the source PRD gives; a rule that wants root files lists `*.md` as well; package 06 fixed `?` as literal for tokens and §8.3.4 gives it a meaning for policy.
3. **Glob validity.** A path glob is 1 to 255 characters and does not start with `/`, as the `paths` of a path-scoped token; a `targets` entry follows the rule of `protected_branches` entries and is checked by the same function of package 11. Why: the PRD says "globs" are validated and gives these rules for the two other glob lists.
4. **`default` is a reserved rule name.** Why: requirements name their rule, and the default requirement is named `default`.
5. **Absent members.** `default`, `rules`, `approvals`, `checks`, `fresh_checks`, `block` and `exclude` may be absent and take the values of the never-set document; `approvers` and `targets`, when present, must not be empty. Why: the PRD gives defaults for these members, and an empty `approvers` or `targets` could match nobody and nothing.
6. **Check names in a policy** follow the name pattern of §8.3.6. Why: a name no report can carry would leave changes pending forever.
7. **`invalid_policy` details.** `details` is `{rule, index, field}` and only the first violation is reported. Why: the PRD says the error names the rule and field, and clients must not parse `message`.
8. **Size of a document** is the byte length of the JSON text sent: the body of `PUT`, the `policy` member of `evaluate`. Why: it is the only size the caller can know beforehand.
9. **`user:<username>`** is valid when a user record with exactly that username exists, whatever its status. Why: the PRD says "existing usernames" and that later deletion does not invalidate the text.
10. **Revisions.** Revision 0 is never stored; it is readable only while it is the current one. Revisions beyond the 50 highest are dropped in the `PUT` that writes a new one. Why: no separate maintenance task is needed, and the PRD's task list of the maintenance loop has none for policy.
11. **`If-Match`** accepts the revision bare or in double quotes. Why: the PRD writes `If-Match: <revision>` and returns `ETag: "<revision>"`.
12. **Order of `PUT` refusals:** fork, validation, archived, revision. Why: the global order puts validation before state; the fork refusal is a `400` that needs no body.
13. **An archived workspace** refuses `PUT` with `409 workspace_archived` (WS-FR-059: no writes) and still answers `GET` and `evaluate`. Other statuses accept all three routes, because none needs the repository unless `evaluate` names a change.
14. **A fork's policy routes.** `GET` and `evaluate` on a fork's slug use the revision-0 document. Why: a fork has no policy of its own, and a change whose target is the fork is governed by its target workspace's policy, which is the never-set one.
15. **Re-evaluation after `PUT` is synchronous and in the `PUT`'s transaction.** Why: it is the simplest behaviour that is crash consistent: either the new revision and every new `landing` exist, or none does.
16. **Who emits `parley.change.ready`.** `parley.change.ready` has one writer, the landable announcement of package 23 (`internal/change/landable.go`): it writes the event when a change is `open`, its `landing.status` is `ready` and its `mergeable` is `clean`, for a head it has not been announced for since it last was not landable, and forgets the announcement when the change stops being landable. Package 23 runs it after each mergeability result and each of its own edits; this package runs it after every `landing` it stores (a single re-evaluation and each change of a policy update), in the same transaction. Why: the event's condition has two inputs with two owners, and one writer that remembers the announced head is what makes each transition to landable produce exactly one event, whichever input moved last. Package 29 starts automatic lands from that event.
17. **Structural reasons.** `draft` comes from the change's state; the other three are read from the change record, where packages 22 and 23 derive them. Requirements are still computed and listed. Why: §7 fixes when `nothing_to_land` is derived, and that moment is not an evaluation.
18. **Evidence source.** Approvals and checks are asked per requirement through one interface declared in `internal/policy/ports.go`; the implementation installed at first returns nothing, and a registration function replaces it, which package 28 calls with its own. Why: the counting and carry-over rules belong to package 28, which is built after this one.
19. **Order and cut of a requirement's `paths`:** ascending byte order, first 50. Requirements in document order, `default` last. Why: the PRD gives the cap and no order; a fixed order makes answers comparable.
20. **`evaluate`.** `policy_revision` is the stored revision also with a candidate; structural reasons are not applied; `target_branch` together with `change` is refused; `paths` holds at most 2 000 entries (the cap of a change's `paths`); a fork-bound token gets `404 change_not_found` for a change of another source, as on the change routes (CHG-FR-040).
21. **CLI file format** is chosen by the `.toml` suffix, otherwise JSON first and TOML second; TOML is read with `github.com/BurntSushi/toml`, the module packages 03 and 09 already use. `--paths` is a comma-separated list and may be repeated.
22. **Profile: full.**
23. **The land gate is registered, not called.** Package 25 declares the policy gate in `internal/land` and is built before this package; this package registers a gate there that runs the re-evaluation of the change and answers its status, reason, requirements and policy revision. It knows nothing of `override`: package 29 registers the override rule beside it.

## Dependencies

- Package 22 `change_opening_and_versions` (draft): the change record this package reads (state, target branch, current version, touched files, `mergeable`, structural reason) and writes (`landing`), and the landing evaluator registration this package fills.
- Package 23 `change_lifecycle_and_mergeability` (draft): the landable announcement, the one writer of `parley.change.ready`.
- Package 25 `optimistic_landing` (draft): the policy gate registration point of `internal/land`.
- Package 11 `workspace_read_update_settings` (draft): the branch-glob validation and matcher in `internal/workspace/branchglob.go`.

Used through their published components, without being dependencies of this package's plan: package 02 (routes, envelope), 07 (access), 08 (binding guard), 05 (user lookup), 09 (CLI helpers), 52 (event component).

## Verified External API

Read in the module cache:

- `github.com/BurntSushi/toml` v1.6.0 (`decode.go`): `func Unmarshal(data []byte, v any) error`.
- `github.com/spf13/cobra` v1.10.2: `func (c *Command) AddCommand(cmds ...*Command)` (`command.go`), `func (c *Command) Flags() *flag.FlagSet` (`command.go`), `func (c *Command) MarkFlagRequired(name string) error` (`shell_completions.go`), `func ExactArgs(n int) PositionalArgs` (`args.go`).
- `github.com/spf13/pflag` v1.0.10: `func (f *FlagSet) StringVar(p *string, name string, value string, usage string)`, `func (f *FlagSet) StringSliceVar(p *[]string, name string, value []string, usage string)`, `func (f *FlagSet) IntVar(p *int, name string, value int, usage string)`, `func (f *FlagSet) Changed(name string) bool`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Policy**: A workspace's ordered rules that map touched paths to landing requirements.
- **Requirement**: What one selected rule, or the default, demands of one change: approvals, checks or a block, with whether it is satisfied.
- **Landing status**: `ready`, `pending` or `blocked`, the result of evaluating policy for a change.
- **Approval**: A user's verdict (`approve` or `reject`) on one version of a change.
- **Check**: A named result (`pending`, `passed`, `failed`) reported on one version by an external runner.
- **Evidence source**: The interface through which this package reads approvals and check results.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
