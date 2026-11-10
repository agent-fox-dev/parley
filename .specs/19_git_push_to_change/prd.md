---
spec_id: "19"
spec_name: "git_push_to_change"
title: "Turn a push into a change"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "e35a72f441b1484ff9a0ad15bc07e99347c7d954c4a309483e1557062bb8ac31"
schema_version: 3
source: "docs/prd/05_git_server.md"
---

# Turn a push into a change

## Intent

An agent that pushes a branch with stock git should not need a second call to put its work up for landing. Parley therefore turns an accepted push into change activity itself: a push to a branch that already has a change adds a version to it, a push that asks for a change (or lands in a task fork that opens changes automatically) opens one, and the git client is told on its own terminal which change and which version the push produced, or why no change was opened. A problem with the change never costs the agent its push.

## Goals

- Every created or updated branch of an accepted push is mapped, in command order and before the push is answered, to exactly one outcome: a new version of its `draft` or `open` change, a newly opened change, a warning that no change was opened, or nothing.
- The six `parley.*` push options (`parley.change`, `parley.target`, `parley.title`, `parley.intent`, `parley.draft`, `parley.session`) are understood; an invalid one refuses the whole push before the pack is received, with one line that names the option and the reason, and nothing is written.
- For each mapped branch the git client prints one `remote:` line in a fixed form: `change <workspace>#<number> version <n>`, `change <workspace>#<number> version 1 (opened)`, `change <workspace>#<number> version 1 (opened, draft)` or `warning: no change for <branch>: <reason>`.
- A failure to open a change or to record a version never fails the push: the refs stay updated, the ref is reported `ok`, `parley.git.push` is emitted, and the reason is on the `warning:` line.
- `parley.session` attributes every push to a session: the push events, the new versions and the opened changes carry it, whether or not a change is involved.
- A push to a task fork with `auto_change` on opens a change in the fork's parent without any option, also from a git client that cannot send push options.

## Non-goals

- The three git routes, Basic authentication, the order of checks before the exchange, the capability advertisement (including `push-options` and `side-band-64k`) and `parley credential-helper`: package 17 `git_smart_http`.
- Reading the command list and the pack, quarantine, the push rules (hub-owned refs, protected branches and the owner exemption, tracking branches, path-scoped tokens), atomic compare-and-swap of refs, per-ref refusal reporting, the `parley.git.push` event and its fields other than `session_id`, and the `parley_git_requests_total` metric: package 18 `git_push_rules`. This package registers the options stage and the mapping stage that package declares in its push processing, and hands it the session id.
- What opening a change and recording a version do (validation at open, defaults for title, intent, target and session, version refs, the stack link, policy evaluation, the mergeability check, the events `parley.change.version` and `parley.change.open`), abandoning a change whose source branch a push deleted (CHG-FR-013), and recording the version of a push to the source branch of a `draft` or `open` change: package 22 `change_opening_and_versions`. It records that version in a ref-updated stage of package 18's ref writer, in the transaction of the ref update (CHG-FR-010); this package calls it to open changes and reports what was recorded.
- Editing an existing change (title, intent, target, draft state): package 23 `change_lifecycle_and_mergeability`. Push options never do this.
- Restack after a new version: package 24 `change_stacks_and_restack`. Overlap detection after a new version: package 30 `awareness`. Both run after the push is answered.
- The path check of a path-scoped token on the version a push creates or the change it opens (GIT-FR-032, LAND-FR-080): packages 18 `git_push_rules` and 29 `auto_land_revert_and_path_limits`.
- Opening a change from an API commit (`change` object, `auto_change` over the API): package 21 `scm_write_api`.
- Carry-patch push control and what follows a patch branch push: package 37 `carry_patch_push_control`.
- The session record, its routes and the session-linking check itself: package 49 `agent_sessions_and_usage`. This package reaches the check through the interface `SessionLinker` it declares in `internal/gitserver`.
- The fork's `auto_change`, `purpose` and `fork_point` fields: package 15 `task_fork_creation`. This package reads them.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/05_git_server.md`, GIT-FR-040 to GIT-FR-054, with the edge cases of §5.10 that concern push options and change mapping. What opening a change and recording a version mean is in `docs/prd/07_changes_and_stacks.md`, CHG-FR-001 to CHG-FR-013; the session-linking rule is in `docs/prd/14_sessions_audit_events.md` §14.3.2; `auto_change` is WS-FR-075 of `docs/prd/04_workspaces_and_forks.md`.

A push is processed in the order of GIT-FR-020: read the commands and push options → validate the push options → receive the pack into quarantine → evaluate the push rules for every ref → update all refs together → map branches to changes → emit events → answer. Package 18 builds that pipeline in `internal/gitserver/` and declares two stage interfaces in it. This package registers the options stage (the second step) and the mapping stage (the sixth step, with its client-visible lines), and passes the session of the push to the fifth and the seventh. Inside the fifth step, package 22's ref-updated stage records the version of every pushed branch that is the source of a `draft` or `open` change; the mapping stage reads that result and opens the changes that are still to be opened.

The code lives in `internal/gitserver/`: `pushoptions.go` (parsing and validation of push options), `changemap.go` (the per-branch decision and the calls into `internal/change`), `pushreport.go` (the side-band lines), each with its test file beside it, plus `pushchange_integration_test.go`, which drives a real `git` client against an in-process hub. The push processor that package 18 writes in `internal/gitserver/` calls them through its two stage interfaces; this package does not edit it. End-to-end tests are in `test/e2e/push_change_test.go`.

## Requirements

### Push options (GIT-FR-050, GIT-FR-051)

A git client sends push options with `git push -o <option>`. The hub understands six. Options whose name does not begin with `parley.` are ignored.

| Option | Value | Effect |
|---|---|---|
| `parley.change` | none | Open a change for every pushed branch that is not already the source of an `open` or `draft` change |
| `parley.target=<branch>` | branch name | Target branch of the changes this push opens. Implies `parley.change` |
| `parley.title=<text>` | 1–200 characters | Title of the changes this push opens |
| `parley.intent=<text>` | up to 1,024 bytes | Intent of the changes this push opens |
| `parley.draft` | none | Open the changes in state `draft` instead of `open` |
| `parley.session=<id>` | session id | Session recorded on the push events, on new versions and on opened changes |

An option is split at its first `=` into a name and a value, so `parley.title=a=b` gives the title `a=b`. A push carries at most 16 options of at most 1,024 bytes each; both limits count every option, with or without the `parley.` prefix, and the byte limit counts the whole option, name and value.

Options are validated before the pack is received. The first invalid option, in the order sent, refuses the whole push. The git client prints `remote: error: invalid push option <name>: <reason>`, every ref of the push is reported `! [remote rejected] <ref> (invalid push option <name>: <reason>)`, and `git push` exits non-zero. The HTTP status is 200. Nothing is written: no object is kept, no ref moves, no event is emitted, no change is touched, and the request counts as `refused` in `parley_git_requests_total`. The reasons:

| Invalid because | `<reason>` |
|---|---|
| a `parley.` name that is not one of the six | `unknown option` |
| the same `parley.` option given twice | `given more than once` |
| `parley.change` or `parley.draft` written with `=` | `takes no value` |
| `parley.target`, `parley.title`, `parley.intent` or `parley.session` without `=` or with an empty value | `value required` |
| a `parley.target` for which `refs/heads/<value>` is not a well-formed ref name by git's rules | `not a valid branch name` |
| a `parley.title` longer than 200 characters | `longer than 200 characters` |
| any option longer than 1,024 bytes | `longer than 1024 bytes` |
| a `parley.session` that fails the session-linking rule | `unknown session` |

A push with more than 16 options is refused in the same way with the line `error: too many push options: at most 16 are accepted`. If the session cannot be looked up because of an internal failure, the push is refused with `error: internal error; try again`; validation never fails open.

`parley.session` is accepted when the session exists, was created by the caller's user (or the caller is the admin token), and the session's workspace is the pushed workspace, its parent or one of its forks. The session's status is not checked.

A client that does not support push options sends none; that is a valid push with no options.

### The session of a push (GIT-FR-054)

`parley.session` takes effect on every accepted push, whether or not a change is involved. Each `parley.git.push` event of the push carries the session id in `session_id` (null when the option is absent). Each version the push records (through the ref writer of package 18, which hands the session to the ref-updated stage) and each change it opens is given the session id. Without the option the mapping passes no session, and package 22's default applies to an opened change (the session recorded on the source fork, CHG-FR-004).

### Mapping branches to changes (GIT-FR-040)

After the refs of an accepted push are written and before the push is answered, the hub looks at each created or updated branch (`refs/heads/*`), in command order. Tags, refs in other namespaces and deletes are skipped: they get no line and cause no change activity. For each branch, the first of these that applies decides:

1. The branch is the source of a change in state `open` or `draft`: a new version of kind `push` is recorded for that change. No option is needed, and only `git:write` is needed. The version is recorded by the ref-updated stage of package 22 inside the commit step of the ref update, so it exists when the mapping runs; the mapping takes it from the ref writer's result and only words the line.
2. The branch is a protected branch that this push moved by a direct push (the owner exemption, GIT-FR-034): no change is opened. When the push carries `parley.change` or `parley.target`, the client gets `warning: no change for <branch>: branch is protected and was pushed directly`; otherwise it gets no line.
3. The push carries `parley.change` or `parley.target`, or the pushed workspace is a task fork with `auto_change` on: a change is opened with the branch as its source.
4. Otherwise nothing happens. The branch is just a branch, and the client gets no line.

A branch whose earlier change has landed or been abandoned is not the source of an open change, so rule 3 opens a new change for it. `parley.change` on a push that updates only tags or only deletes branches has no effect and is not an error. A push in which any ref was refused, and a push with no commands, cause no mapping at all.

If two pushes race to open a change for the same branch, the one that loses finds the change already there (`change_exists`); it records a version on that change instead.

### What an opened change is (GIT-FR-041, GIT-FR-052)

The change belongs to the workspace of its target branch. For a push to a repository workspace that is the pushed workspace. For a push to a task fork it is the fork's parent, and the source is `{fork slug, branch}`. The target branch is the value of `parley.target` when given; otherwise the default of CHG-FR-004 applies: the fork's base branch (`fork_point.ref`) in a fork, the workspace's default branch elsewhere.

`parley.title` and `parley.intent` are passed as given. When absent, the defaults of CHG-FR-004 apply: the title is the subject line of the branch's head commit, the intent is the body of that commit's message and then the `purpose` of the source fork. `parley.draft` opens the change in state `draft`. The author of the change is the pushing credential's actor.

`parley.target`, `parley.title`, `parley.intent` and `parley.draft` affect only changes opened by this push. When a push opens several changes, the options apply to each. They never alter an existing change: a push with these options to a branch that is already the source of an `open` or `draft` change records a version and leaves that change's title, intent, target branch and state as they were.

### What the git client sees (GIT-FR-042)

The mapping runs before the push is answered, so its result can be reported. For each mapped branch the client receives one side-band line, which git shows with the `remote:` prefix, in command order:

| Outcome | Line |
|---|---|
| New version of an existing change | `change <workspace>#<number> version <n>` |
| Change opened | `change <workspace>#<number> version 1 (opened)` |
| Change opened as draft | `change <workspace>#<number> version 1 (opened, draft)` |
| Change not opened | `warning: no change for <branch>: <reason>` |

`<workspace>` is the slug of the workspace the change belongs to (the parent's slug for a push to a fork). `<branch>` is the ref name without `refs/heads/`. The lines carry no URL. A client that did not negotiate a side band gets no lines; the mapping happens all the same.

```
$ git push -o parley.change -o parley.title="Fix retry loop" origin fix-retry
remote: change demo#12 version 1 (opened)
To https://hub.example/git/acme/demo.git
 * [new branch]      fix-retry -> fix-retry
```

### Failures never fail the push (GIT-FR-043, GIT-FR-053)

A failure in the mapping never fails the push: the refs stay updated, the ref is reported `ok`, `parley.git.push` is emitted, `git push` exits 0, and the reason is on the `warning:` line. A failure for one branch does not stop the mapping of the other branches of the push. The reasons:

| Failure | `<reason>` |
|---|---|
| the target branch does not exist (also a `parley.target` that names no branch; this is not an option validation error) | `target branch <target> does not exist (target_not_found)` |
| the branch is its own target | `branch is its own target` |
| the branch has no commits ahead of the target | `no commits ahead of <target> (nothing_to_land)` |
| a merge commit among the commits ahead of the target | `merge commits are not supported (merge_commits_unsupported)` |
| a PAT without `changes:write` on a push that would open a change | `token lacks scope changes:write` |
| an empty intent while the workspace setting `changes.require_intent` is true and `parley.draft` was not given | `intent is required; push with -o parley.intent=<text> or -o parley.draft (intent_required)` |
| any other refusal of the open, with message M and `error_type` T | `M (T)` |
| an internal failure while opening the change, or while recording a version after a lost race to open | `internal error` |

Recording a version on an existing change needs only `git:write`: a PAT without `changes:write` that pushes to the source branch of an open change gets the version line, not a warning. That version is part of the ref update: if the ref-updated stage fails, package 18 refuses the push with `internal error; try again` and no ref moves, so a branch never moves without its version.

### End-to-end flows

- **Open, then add a version.** An agent creates a branch in a clone of workspace `demo`, commits, and runs `git push -o parley.change -o parley.title="Fix retry loop" origin fix-retry`. `parleyd` validates the options, receives the pack, applies the push rules, writes the ref, opens change `demo#1` with version 1 and answers. The git client prints `remote: change demo#1 version 1 (opened)`. The agent commits again and runs `git push origin fix-retry` without options; the client prints `remote: change demo#1 version 2`, and `GET /workspaces/demo/changes/1` shows `version: 2` with the pushed head.
- **Automatic change from a fork.** An agent clones a task fork of `demo` that has `auto_change` on, commits on a new branch and runs `git push origin work` with no option. `parleyd` opens a change in `demo` whose source is the fork's branch and whose target is the fork's base branch. The client prints `remote: change demo#<n> version 1 (opened)`.
- **Invalid option.** A developer runs `git push -o parley.titel=x origin topic`. `parleyd` refuses the push before reading the pack. The client prints `remote: error: invalid push option parley.titel: unknown option` and exits non-zero; the branch does not exist on the hub.

## Design Decisions

1. **Language and tools.** Go; git operations run the `git` binary (2.38 or later) through `internal/gitrepo`; HTTP is the standard library `net/http`; tests use the standard `testing` package, table-driven, with repositories made by `git init` in `t.TempDir()` and a real `git` client for pushes. These are project-wide decisions.
2. **The mapping calls package 22 through a narrow interface declared in `internal/gitserver`.** It needs three operations: find the `draft` or `open` change whose source is a given workspace and branch; record a version of kind `push` for a change (only after a lost race to open); open a change from a source branch with optional target, title, intent, draft flag and session. The ordinary version of a push is not recorded through this interface: package 22 records it in the ref-updated stage it registers with package 18 (decision 16 of package 18, decision 6 of package 22), and the mapping reads the result. The implementation is `internal/change`; unit checks use a fake. Why: package 22 is specified separately, and the mapping's decisions can be tested without it.
3. **Session validation goes through `SessionLinker`, an interface declared in `internal/gitserver`** with one operation (caller, session id, workspace → accepted, refused or internal error). The check is package 49's; packages 21 and 22 declare an interface of the same name and operation in `internal/scm` and `internal/change`, and package 49 hands its one check to all three. The interface of this package stays check-only: it records no session link, because a push becomes a session link only through the change version that package 22 records for it (package 22's `SessionLinker` alone has a record operation, called in the transaction that records the version or opens the change). While no linker is registered every `parley.session` is refused with `unknown session`. Why: the rule is one rule (§14.3.2), and the plan gives this package no dependency on package 49.
4. **Wording of the option reasons.** The PRD fixes the line `error: invalid push option <name>: <reason>` but not the reasons. The eight reasons in the table above are fixed here so that tests and agents can match them.
5. **A refused push for an invalid option is reported as `unpack ok`, then `ng <ref> invalid push option <name>: <reason>` for every ref, and the `error:` line once on the side band.** The pack bytes are read and thrown away without being stored. Why: this is how git itself reports a push declined before its refs are updated, and it gives the client the per-ref and the `remote:` form of GIT-FR-023.
6. **More than 16 options has its own line**, `error: too many push options: at most 16 are accepted`, because no single option is at fault. The limits count all options, not only `parley.*` ones.
7. **The 1,024-byte limit of one option counts name and value.** The separate 1,024-byte limit of the `parley.intent` value can therefore never be the one that trips; one reason, `longer than 1024 bytes`, covers both.
8. **Title length is counted in Unicode characters**, as the change's `title` field is (1–200 characters).
9. **Wording of the warning reasons.** The PRD lists the failures but not their text. Each reason that corresponds to an `error_type` of §7 ends with that type in parentheses, so that an agent can match on it.
10. **The scope check for opening is made by the mapping**, before it calls the open operation: only a PAT is scope-checked, and it needs `changes:write`. Why: the open operation of package 22 is also reached from routes whose scope check is done by middleware.
11. **A failure to record a version after a lost race uses the same `warning: no change for <branch>: <reason>` line**, with the reason `internal error`. The PRD defines no other line, and start-up reconciliation (§7.7) records the missed version later. The ordinary version of a push cannot fail this way: it is recorded inside the ref update (package 18), and its failure refuses the push before a ref moves.
12. **A lost race to open a change becomes a version.** When the open is refused with `change_exists`, the mapping records a version on the existing change instead of warning. Why: that is what rule 1 would have done had the other push finished first.
13. **When the branch's head is already the head of the change's current version, no version is recorded and no line is sent** (§7.3.2: such a move records nothing). The mapping sees this as a branch with a `draft` or `open` change for which the ref writer reported no version.
14. **A direct push of a protected branch is skipped without a line when the push has neither `parley.change` nor `parley.target`**, also in a task fork with `auto_change`: GIT-FR-040 gives the warning only for a push that asked for a change.

## Dependencies

- Package 18 `git_push_rules` (draft): the push pipeline with its options stage and mapping stage interfaces: the raw push options, the accepted ref updates in command order with their `direct` flag and the version a ref-updated stage recorded, the side band, the status report of a refused options stage, and the `parley.git.push` event that receives the session id.
- Package 22 `change_opening_and_versions` (draft): opening a change, recording a version, and the lookup of the open or draft change of a source branch.
- Package 52 `durable_event_log_and_stream` (draft): the event log the integration tests read.
- Through an interface, not a dependency: package 49 `agent_sessions_and_usage` (the session-linking check behind `SessionLinker`).

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token. Nobody can force-push or delete it. By default the default branch.
- **Direct push**: A fast-forward update of a protected branch by the workspace's owner or the admin token, over git or the source control API, without a change. Allowed while the workspace setting `protect.owner_push` is true.
- **Change**: The unit of work: a proposal to add the commits of a source branch to a target branch, with a number, a title, an intent, versions and a state.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Intent**: The author's statement of why the change exists.
- **Session**: An agent session: who ran, on which workspace, with what token usage, and which changes it produced.
- **Push option**: A string a git client sends with a push (`git push -o <option>`), delivered to the server before the pack.
- **Side band**: The channel of the git protocol on which the server sends text that the client prints with the `remote:` prefix.
