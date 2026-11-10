---
spec_id: "28"
spec_name: "approvals_and_checks"
title: "Record approvals and check results on versions"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "5919b2581f57bbe4db966f359e6c28b49aa8de418526633c9736fb2f7924ed1e"
schema_version: 3
source: "docs/prd/08_landing_and_policy.md"
---

# Record approvals and check results on versions

## Intent

Landing in Parley is not serialized, so its safety rests on policy, and policy rests on two kinds of evidence about one version of a change: a person's or a review agent's verdict, and the named results of external check runners. The hub records both against the exact version they were given on, decides which verdicts count so that nobody certifies their own push with the credential that made it, keeps the evidence across a pure rebase and drops it on a content change, and tells policy every time the evidence changes.

## Goals

- `POST /workspaces/:slug/changes/:number/approvals` records one verdict (`approve` or `reject`) per user per version; a second verdict by the same user on the same version replaces the first.
- `GET …/approvals` lists verdicts newest first; `DELETE …/approvals/:id` lets the verdict's author, the workspace owner or the admin token withdraw one.
- An approval counts toward a policy requirement only when its user is among the requirement's approvers and its credential created no version of the change; each approval object names the requirements it counts toward in `counts_for`.
- A new version that is patch-equivalent to an earlier one receives that version's verdicts with `carried_from` set; any other new version starts with none.
- `PUT …/checks/:name` records one result (`pending`, `passed`, `failed`) per version and name; `GET …/checks` lists the results that apply to a version, carried-over ones included.
- The landing evaluator of package 27 receives, for every requirement, the number of counted approvals, whether a counted rejection is outstanding, and the status of every named check, with `fresh_checks` honoured.
- Every recorded verdict, withdrawal and check report re-evaluates the change's `landing` in the same store transaction, and recording emits `parley.change.approve`, `parley.change.reject` or `parley.change.check`.
- `parley change approve|reject|approvals|check|checks` call these routes.

## Non-goals

- The policy document, its validation and revisions, rule selection, the requirement object, the computation of `satisfied` and `landing.status`, the re-evaluation triggers other than the three writes of this package, `GET/PUT …/policy`, `POST …/policy/evaluate` and `parley policy …`: package 27 landing_policy. This package registers itself as the evidence source that package 27 declares in `internal/policy/ports.go` (per requirement: counted approvals, an outstanding counted rejection, a status per named check) and calls its re-evaluation.
- The change object, versions, `patch_equivalent_to` and how patch equivalence is computed, `created_by`, the `include` parameter of `GET …/changes/:number`, and the `parley change` command group itself: package 22 change_opening_and_versions. This package registers a version observer and the two include expanders `approvals` and `checks` with it, and takes each version's creating credential from what the observer is handed.
- Landing, the policy gate at landing and `409 policy_blocked`: package 25 optimistic_landing. Override of unmet approvals and checks (LAND-FR-002), automatic landing on `parley.change.ready`, revert and path-scoped tokens at landing: package 29 auto_land_revert_and_path_limits.
- The `parley.change.ready` event: its one writer is the landable announcement of package 23 change_lifecycle_and_mergeability, which the re-evaluation of package 27 landing_policy runs in the transaction that stores a `landing`. This package writes no event of that type; a verdict, a withdrawal or a check report that makes a change landable yields exactly one through that re-evaluation.
- The binding guard that answers `403 token_bound` and the table of routes a fork-bound token reaches on the fork's parent: package 08 workspace_bound_tokens. This package only confines the two read routes that the guard lets through to changes whose source is the fork.
- The scope registry and the rule that `changes:approve` and `checks:write` are implied by no other scope: package 06 api_keys_and_access_tokens. The access rule (`404 not_found`, `403 forbidden`): package 07 workspace_access_rule.
- The error envelope, the order of checks, list paging and `Idempotency-Key` (PLAT-FR-010 to PLAT-FR-019): package 02 http_api_conventions. CLI settings, output and exit codes (PLAT-FR-090 to PLAT-FR-095): package 09 cli_foundation.
- Storing events, `seq`, visibility and the stream: package 52 durable_event_log_and_stream. This package hands events to the event writer the change component uses.
- Running checks. External runners run them and report the result; the hub never times a check out (§8.10).
- Paging flags (`--limit`, `--cursor`) on `parley change approvals` and `parley change checks`, and a `--details` flag on `parley change check`: the source PRD lists none; not covered by this package.

## Background

The repository holds no code yet; every file named here is new, except that the `change` command group of the CLI is first created by package 22 in `internal/cli/change.go` and the Go package `internal/policy` is first created by package 27. The source is `docs/prd/08_landing_and_policy.md`: §8.3.5 (approval), §8.3.6 (check), LAND-FR-050 to LAND-FR-053, the five routes of §8.5, the five commands of §8.6, the three events of §8.8 and the edge cases of §8.10 that concern approvals and checks (user stories LAND-US-05 and LAND-US-06). The fork-bound-token sentences of LAND-FR-003 that name these routes are covered here too.

Package 22 delivers the change with its versions; each version carries `patch_equivalent_to` and `created_by`. Package 27 delivers the policy and an evaluator that turns a change into requirements, each `{rule, paths, approvals: {required, counted, approvers}, checks: [{name, status}], block, satisfied}`, stored on the change as `landing`. Until this package exists that evaluator has no evidence: no approval is counted and every named check is `pending`. This package supplies the evidence.

The code lives in `internal/policy/` beside the policy code (`approval.go`, `approval_routes.go`, `counting.go`, `carry.go`, `check.go`, `check_routes.go`, `facts.go`, each with its test file) and in `internal/cli/change_review.go` (the five commands). The end-to-end test is `test/e2e/approvals_checks_test.go`. Two store tables (verdicts, check results) and one table of version creators are added through the schema migration of `internal/store/`.

## Requirements

All routes are under `/api/v1/workspaces/:slug`, where `:slug` is the workspace that holds the change's target branch.

### The approval object (§8.3.5)

`{"id", "change", "version", "verdict", "comment", "by", "counts_for", "carried_from", "created_at"}`. `id` is a UUID. `change` and `version` name the version the verdict is on. `verdict` is `approve` or `reject`. `comment` has at most 16,000 characters and is `""` when none was given. `by` is `{actor_type, actor_id, username}` of the credential that gave the verdict (`actor_type` is `api_key` or `pat`). `counts_for` lists the names of the requirements the verdict currently counts toward; it is computed when the object is read. `carried_from` is the version the verdict was first given on when it was carried over, else null.

### Recording a verdict (LAND-FR-050, LAND-FR-051, LAND-FR-053)

`POST …/changes/:number/approvals` with `{"verdict": "approve" | "reject", "version"?: int, "comment"?: string}`.

- The caller needs read access to the workspace and, for a PAT, the scope `changes:approve`. A PAT without it, including one that holds `changes:write`, gets `403 insufficient_scope`. A caller without access gets `404 not_found`. Any user who can read the change may record a verdict, an org member included; whether it counts is a separate question.
- The admin token has no user and gets `403 forbidden`.
- A token bound to a fork gets `403 token_bound` on the fork's parent (LAND-FR-003): a fork token cannot certify its own work.
- Validation, `400 invalid_request`: a missing or unknown `verdict`; a `comment` of more than 16,000 characters; a `version` that is not a positive integer; an unknown member; a body that is not a JSON object.
- An unknown change is `404 change_not_found`. A `version` that names no version of the change is `404 not_found`.
- A `landed` or `abandoned` change answers `409 change_closed`. A `draft` change accepts verdicts.
- Without `version` the verdict is on the current version.
- A user has at most one verdict per version. The first is answered `201` with the approval; a later one by the same user on the same version replaces it and is answered `200`. The replacement is a new verdict: new `id`, new `created_at`, `carried_from` null.
- A verdict on a version that is neither the current version nor patch-equivalent to it is recorded and has no effect on landing; its `counts_for` is empty.
- A verdict on an earlier version to which the current version is patch-equivalent is recorded on that version and carried to the current version in the same step.
- Recording emits `parley.change.approve` or `parley.change.reject` (`resource_type` `change`, `resource_id` the number, actor the caller, metadata `version`, `approval_id`, `counts_for`) and re-evaluates the change's policy.

### Listing and withdrawing verdicts

`GET …/changes/:number/approvals` needs `changes:read` and read access. Query `version` (default: every version). The answer is a list (`{"items", "next_cursor"}`) of approval objects, newest first. A `version` that is not a positive integer is `400 invalid_request`; one that names no version is `404 not_found`.

`DELETE …/changes/:number/approvals/:id` needs `changes:approve` (PATs) and read access. Only the verdict's author (the same user, with any credential), the workspace owner or the admin token may withdraw; anyone else gets `403 forbidden`. An `:id` that names no verdict of this change is `404 not_found`. The answer is `204` with no body. The verdict is removed, the change's policy is re-evaluated, and no event of its own is written. A fork-bound token gets `403 token_bound`.

A token bound to a fork F reaches `GET …/approvals` and `GET …/checks` on F's parent only for changes whose source workspace is F; another change answers `404 change_not_found` (LAND-FR-003).

### The counting rule (§8.3.5)

An approval counts toward a requirement when both hold:

1. its user is among the requirement's `approvers`: `owner` is the workspace's owner, `member` is any member of the workspace's organization, `user:<username>` is that user;
2. it was not given with a credential that created any version of the change, the approved one or an earlier one. Credentials are compared, not users: a person approving their agent's work with their own API key counts; an agent approving the version it just pushed does not; a person who pushes with one token and approves with another counts (§8.10, self-approval through a second token). Versions created by the hub (`created_by.actor_type` `system`: restack, sync) have no creating credential and change nothing.

Only verdicts on the current version are considered, carried ones included. The evaluator receives per requirement the number of counted `approve` verdicts and whether a counted `reject` is outstanding. A counted `reject` keeps its requirement unsatisfied until its author replaces or withdraws it, or a version that is not patch-equivalent arrives. `counts_for` on an approval object lists the `rule` of every requirement of the change's current `landing` for which the verdict counts, in the order of the requirements; for a verdict that is not on the current version it is empty.

### Carry-over of verdicts (§8.3.5, CHG-FR-011)

When a version is recorded with `patch_equivalent_to` naming an earlier version, every verdict on that earlier version is copied to the new version in the transaction that records the version. The copy keeps verdict, comment, author, credential and `created_at`, gets a new `id`, and has `carried_from` set to the version the verdict was first given on (the source's `carried_from` when it has one, else the source's version). A version with `patch_equivalent_to` null starts with no verdicts, so approvals and outstanding rejections given on earlier versions no longer apply. Carry-over writes no event.

### The check object (§8.3.6)

`{"name", "change", "version", "status", "summary", "url", "details", "reported_by", "updated_at"}`. `name` matches `^[a-z0-9][a-z0-9_.-]{0,62}$`. `status` is `pending`, `passed` or `failed`. `summary` has at most 2,000 characters. `url` is an optional link to the run. `details` is an optional JSON object of at most 16 KiB. `reported_by` is `{actor_type, actor_id, username}`. There is one result per version and name; a later report replaces it.

### Reporting a check (LAND-FR-052, LAND-FR-053)

`PUT …/changes/:number/checks/:name` with `{"status", "version"?, "summary"?, "url"?, "details"?}`.

- The caller needs write access to the workspace and, for a PAT, `checks:write`. A PAT whose only scope is `checks:write`, owned by a user with write access, can report and can do nothing else (LAND-US-06). A PAT without the scope gets `403 insufficient_scope`; a read-only member gets `403 forbidden`; a caller without access gets `404 not_found`; a fork-bound token gets `403 token_bound`. The admin token may report.
- Validation, `400 invalid_request`: a `:name` that does not match the pattern; a missing or unknown `status`; a `summary` of more than 2,000 characters; a `url` that is not an absolute `http` or `https` URL or has more than 2,048 characters; `details` that is not a JSON object or whose JSON text is larger than 16,384 bytes; a `version` that is not a positive integer; an unknown member.
- An unknown change is `404 change_not_found`; a `version` that names no version is `404 not_found`; a `landed` or `abandoned` change is `409 change_closed`; a `draft` accepts reports.
- The answer is `200` with the check, for a first report and for a replacement.
- Each report emits `parley.change.check` (metadata `version`, `name`, `status`) and re-evaluates the change's policy.

### Reading checks and the status policy sees (§8.3.6)

`GET …/changes/:number/checks` needs `changes:read` and read access. Query `version` (default: the current version). The answer is a list of check objects ordered by `name`.

The result that applies to a version V for a name is the result reported on V itself; when there is none, the result reported on the nearest earlier version reached from V by following `patch_equivalent_to`. The list holds one object per name that has an applying result; a carried-over result shows the earlier version in its `version` member.

For a requirement, the status of a named check is the applying result's status for the current version, or `pending` when there is none. When the requirement sets `fresh_checks`, only a result reported on the current version itself applies. A report on a version that is neither current nor reached from the current version by `patch_equivalent_to` is recorded and has no effect on landing. A check that policy names and no runner reports stays `pending` for ever.

### Re-evaluation and atomicity (LAND-FR-044)

A recorded verdict, a withdrawal and a check report each re-evaluate the change through the evaluator of package 27 and store the result as the change's `landing`. The write, the new `landing` and the event are one store transaction: when any part fails the answer is `500 internal` and nothing was written. Requests on one change do not fail because another is running: of two concurrent verdicts by one user on one version, or two concurrent reports of one check on one version, both are answered with success and one row remains.

`GET …/changes/:number?include=approvals` adds `approvals`: the verdicts on the current version, newest first. `include=checks` adds `checks`: the results that apply to the current version, by name.

### CLI (§8.6)

| Command | Flags | Call |
|---|---|---|
| `parley change approve <slug> <number>` | `--version <n>`, `--comment <text>` | `POST …/approvals` with `verdict: "approve"` |
| `parley change reject <slug> <number>` | `--version <n>`, `--comment <text>` (required) | `POST …/approvals` with `verdict: "reject"` |
| `parley change approvals <slug> <number>` | `--version <n>`, `--withdraw <id>` | `GET …/approvals`, or with `--withdraw` `DELETE …/approvals/:id` |
| `parley change check <slug> <number> <name>` | `--status pending\|passed\|failed` (required), `--version <n>`, `--summary`, `--url` | `PUT …/checks/:name` |
| `parley change checks <slug> <number>` | `--version <n>` | `GET …/checks` |

Each prints the route's response body on stdout and exits 0; a withdrawal, whose route answers `204`, prints `{}`. A missing `--comment` on `reject`, a missing or unknown `--status`, a `--version` that is not a positive integer, a `<number>` that is not a positive integer, and `--withdraw` together with `--version` are usage errors: exit status 2, nothing on stdout, no request sent.

### End-to-end flows

1. A workspace's policy requires one approval and the check `test`. An agent opens a change; its `landing.status` is `pending`. A review agent runs `parley change check <slug> 1 test --status passed`; `parleyd` records the result, emits `parley.change.check` and re-evaluates: still `pending`. The developer who owns the workspace runs `parley change approve <slug> 1`; `parleyd` records the verdict, finds that it counts, emits `parley.change.approve` and re-evaluates: `ready`. `parley change approvals` and `parley change checks` show the evidence.
2. An agent approves its own change with the token that pushed it; `parleyd` records the verdict and does not count it. The developer approves; it counts. The agent rebases the change without changing its content; `parleyd` records a patch-equivalent version and carries both verdicts and the check result to it, and the change stays `ready`. The agent then commits a content change; the new version starts with no verdicts and no applying check result, and the change is `pending` again.

## Design Decisions

1. **Profile: full.** Two flows cross the CLI, the router, the policy component, the change component and the store.
2. **Go, SQLite through `modernc.org/sqlite` behind `internal/store`, `net/http` routing, `cobra` for the CLI, standard `testing` with `httptest`** (project decisions 1 to 4 and 7).
3. **This package lives in the Go package `internal/policy` with package 27** and supplies the evaluator's evidence by registering its facts source as the evidence source package 27 declares, through that package's registration function, with the question exactly as declared there: per requirement, the number of counted approvals and whether a counted rejection is outstanding; per named check, its status. Without it the evaluator counts nothing and sees every check as `pending`. Why: the layout puts policy, approvals and checks in one place, and package 27 is built first.
4. **The creating credential of each version comes from package 22's version observer.** The observer is handed, with each recorded version, the credential that created it as credential type and credential id (the `key_id` of an API key, the `token_id` of a PAT, `admin` for the admin token; none for a hub-created version). This package keeps it in its own table, written in the observer's transaction, and reads the request's authenticated caller nowhere for this purpose, so that versions recorded by a push, an API commit, a restack and a restore are all covered. Why: the version's `created_by` holds the user, and the counting rule compares credentials. A version whose `created_by.actor_type` is `system` records none. A version that is not hub-created and has no recorded credential is treated as created by every credential of its `created_by` user, so a missing record can only make an approval not count.
5. **"The approved one or an earlier one" is read as: versions numbered at or below the version the verdict is on.** For a carried verdict that is the version it was carried to, so a credential that approves and then pushes a pure rebase stops counting.
6. **Approver forms.** `owner` matches the workspace's owner; `member` matches every member of the workspace's organization, the owner included when a member; `user:<username>` is compared with the username stored on the verdict. An approval of a requirement that needs zero approvals still counts toward it and appears in `counts_for`.
7. **Verdicts are copied at carry-over; check results are not.** A check result stays on the version it was reported on and is looked up through `patch_equivalent_to` when read, because `fresh_checks` must be able to tell the two apart (CHG-FR-011: earlier results belong to their own version).
8. **A verdict recorded on an earlier version in the current version's patch-equivalence chain is also carried to the current version** (replacing the same user's verdict there). Why: LAND-FR-051 gives such a verdict an effect on landing, and only verdicts on the current version are counted.
9. **A carried copy keeps the original's `created_at` and gets a new `id`.** A replacement gets a new `id`; the old `id` then names nothing.
10. **Withdrawal removes exactly the verdict named by `:id`.** Copies of it on other versions are verdicts of their own. Withdrawal writes no event, because the catalogue of §8.8 has none; the re-evaluation may still emit `parley.change.ready`.
11. **Withdrawal on a `landed` or `abandoned` change answers `409 change_closed`.** The PRD says only that verdicts cannot be given there; the evidence of a closed change stays as it was.
12. **Writes on an archived workspace answer `409 workspace_archived`** (WS-FR-059: an archived workspace accepts no writes); reads are served.
13. **Order of the state checks on the three write routes:** the change exists (`404 change_not_found`), body and parameters are valid (`400`), the workspace is not archived (`409`), the change is not closed (`409`), the version exists (`404 not_found`).
14. **Path and query parameters follow package 22:** a `:number` that is not a positive integer is `404 change_not_found`; a `version` that is not a positive integer is `400 invalid_request`; one that names no version is `404 not_found`. An unknown version has no `error_type` of its own in the PRD.
15. **`GET …/checks?version=N` uses the same rule for every N:** results reported on N plus those carried from the versions N is patch-equivalent to.
16. **List orders are total:** approvals by `created_at` descending, then `id` descending; checks by `name` ascending.
17. **`url` must be an absolute `http` or `https` URL of at most 2,048 characters;** the PRD gives no limit. `comment` and `summary` lengths are counted in Unicode code points. `details` is measured as the bytes of its JSON text as received.
18. **Events of this package carry `session_id` null.** A verdict or a check report is not work of the change's session.
19. **The include expansions:** `approvals` is the verdicts on the current version, `checks` the results that apply to it. The PRD names the two includes and no content.
20. **`parley change approvals --withdraw` together with `--version` is a usage error,** since the two select different calls.

## Dependencies

- Package 27 landing_policy (draft): the policy, the requirement object, the evaluator that this package feeds, and the re-evaluation this package calls after each write.
- Package 22 change_opening_and_versions (draft): the change record, versions with `patch_equivalent_to` and `created_by`, the version observer that hands over each version's creating credential, and include expanders.
- External: none beyond the project's own choices. The CLI commands use `github.com/spf13/cobra` through the command tree of package 09.

## Verified External API

- `github.com/spf13/cobra`: **unverified** (not read in this session; the repository has no dependencies downloaded). Assumed: `(*cobra.Command).AddCommand(cmds ...*cobra.Command)`, `(*cobra.Command).Flags() *pflag.FlagSet`, `(*cobra.Command).MarkFlagRequired(name string) error`, `cobra.ExactArgs(n int) cobra.PositionalArgs`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Patch-equivalent**: Said of a version whose diff against its own base is identical to an earlier version's.
- **Landing**: Adding a change's commits to its target branch: a server-side rebase followed by a compare-and-swap of the target ref.
- **Policy**: A workspace's ordered rules that map touched paths to landing requirements.
- **Requirement**: What one selected policy rule (or the default) asks of a change: approvals, checks or a block.
- **Approval**: A user's verdict (`approve` or `reject`) on one version of a change.
- **Counted approval**: An approval whose user is among a requirement's approvers and whose credential created no version of the change.
- **Check**: A named result (`pending`, `passed`, `failed`) reported on one version by an external runner.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
