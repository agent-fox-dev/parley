---
spec_id: "13"
spec_name: "workspace_push_to_origin"
title: "Publish a workspace branch to its origin"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "4619b92edfa0744f4d6fd14de7e6d9cdc037aef97a13405fd2e5ad0bcb8d2aad"
schema_version: 3
source: "docs/prd/04_workspaces_and_forks.md"
---

# Publish a workspace branch to its origin

## Intent

A writer of a repository workspace publishes one branch from the hub to the workspace's origin with one call, for example to publish landed work or to open a pull request on the forge. The push never overwrites commits on origin that the hub has not seen: it is fast-forward only unless forced, and a forced push replaces origin's branch only while that branch is still where the hub last saw it. Every push handed to origin is recorded as a hub event, whether it succeeded or failed and whether or not the caller stayed connected, and no message about it ever carries the origin credential.

## Goals

- `POST /workspaces/:slug/branches/:name/push` sends exactly one branch, `refs/heads/<name>`, to the same branch on origin and answers `200` with `branch`, `sha`, `remote`, `remote_url` and a `result` of `pushed`, `forced` or `up_to_date`.
- Without `force` origin's branch moves only by a fast-forward; otherwise the answer is `409 non_fast_forward` and origin is unchanged.
- With `force` origin's branch is replaced only while it is at the origin head the hub last saw; otherwise the answer is `409 origin_moved` and origin is unchanged. The hub never sends a blind force.
- A forced push of a protected branch is refused with `403 protected_branch`; the integration branch and registered patch branches of a carry-patch workspace are refused with `403 integration_branch` and `403 patch_branch`; a fork is refused with `400 fork_has_no_origin`.
- Every failure of the push itself has its own answer: `422 origin_rejected`, `502 origin_unauthenticated`, `502 origin_forbidden`, `502 origin_not_found`, `502 origin_unreachable`, `502 upstream_error`, `504 origin_timeout` after 2 minutes.
- A successful push records the pushed commit as the origin head of that branch.
- Each push handed to origin writes exactly one `parley.workspace.push` event; a request refused before the push writes none.
- A push runs to completion or to its deadline when the caller disconnects.
- No message and no event of this route contains the origin credential or URL userinfo, and no message is longer than 1024 characters.
- `parley workspace push <slug> <branch> [--force]` does the same from the shell and always receives the hub's answer, including `504 origin_timeout`.

## Non-goals

- The workspace access rule (levels, the `404` for a caller without access, `403 forbidden` for a member on a write route): package 07 workspace_access_rule. The binding guard for bound tokens: package 08 workspace_bound_tokens. The scope check: package 06 api_keys_and_access_tokens. This package declares what the route needs and adds nothing to those rules.
- The error envelope, the order of the platform's checks, body size and content type, and the CLI's output and exit codes (PLAT-FR-010 to PLAT-FR-019, PLAT-FR-090 to PLAT-FR-095): packages 02 http_api_conventions and 09 cli_foundation.
- The workspace record, the branch-name rule (WS-FR-011), the state guard (WS-FR-007), resolution of the origin credential (§4.3.5), the origin heads as refs `refs/remotes/origin/<name>` with the functions of `internal/gitrepo` that read and write them, and the test origin helper: package 10 workspace_create_and_clone. This package calls them.
- The setting key `protected_branches`, its default and validation, and the branch-glob matcher in `internal/workspace/branchglob.go`: package 11 workspace_read_update_settings. Enforcement of protected branches on pushes into the hub: package 18 git_push_rules.
- Sync from origin: package 12 workspace_sync_from_origin.
- The safety push of every branch before archive and reclone (WS-FR-058, WS-FR-063): package 14 workspace_archive_reclone_delete.
- Task forks: packages 15 task_fork_creation and 16 task_fork_lifetime. This package only refuses a fork on its route.
- The patch list of a carry-patch workspace: package 31 carry_patch_setup_and_patches. The pushes by which the integration branch and patch branches do reach origin (CP-FR-050, CP-FR-082, CP-FR-088): packages 34 rebuild_execution and 37 carry_patch_push_control.
- The text scrubber (PLAT-FR-098): package 03 server_lifecycle_and_configuration. Secret storage: package 39 secrets. The event log: package 52 durable_event_log_and_stream.
- Pushing more than one branch in a call, pushing tags, and deleting a branch on origin. Not in the source PRD.

## Background

The repository holds no code yet; every file named here is new. Source: `docs/prd/04_workspaces_and_forks.md`, WS-FR-049 to WS-FR-057 with WS-US-06, the route and command entries in §4.5 and §4.6, the `parley.workspace.push` row of §4.8, the fixed values of §4.9 and the two push entries of §4.10. `docs/prd/08_landing_and_policy.md` §8.3.1 defines when a branch is protected, and `docs/prd/14_sessions_audit_events.md` §14.8 fixes the event's resource.

Package 10 creates workspaces, clones origin and records the origin head of every branch as the ref `refs/remotes/origin/<name>` of the hub's copy. Package 12 keeps those origin heads current. Until this package, nothing travels from the hub back to origin.

Code lives in `internal/gitrepo/` (the push operation and how its outcome is read from `git`), `internal/gitrepo/gitrepotest/` (the test origin, which must now accept pushes), `internal/workspace/` (the route, the checks before the push, the records after it), `internal/cli/workspace.go` (the command) and `test/e2e/`. The push operation in `internal/gitrepo` takes a commit, a branch name, a lease and a deadline, so that packages 14, 34 and 37 can call it for their own pushes.

## Requirements

### The request (§4.5, WS-FR-049)

`POST /api/v1/workspaces/:slug/branches/:name/push`. Scope `workspaces:write`; write access to the workspace (owner or admin token). A member of the organization gets `403 forbidden`; a PAT without the scope gets `403 insufficient_scope`; a caller without access gets `404 not_found`.

`:name` is the branch name as one path segment, with `/` written as `%2F`. It is percent-decoded exactly once: `fix%2F42` names `fix/42`, and `a%2525b` names `a%25b`.

The body is optional: `{"force": <bool>}`, default `false`. No body, an empty body and `{}` mean `false`. Unknown members are ignored. A body that is not a JSON object, and a `force` that is not a JSON boolean (`null` included), answer `400 invalid_request`.

### Checks before the push (§4.5, WS-FR-052, WS-FR-053, WS-FR-061)

After access, the first failing check decides the answer, in this order:

1. The body, as above: `400 invalid_request`.
2. The decoded branch name is a valid branch name (WS-FR-011): else `400 invalid_request`.
3. The workspace status (WS-FR-007): `pending`, `cloning` or a running reclone, archive or delete answers `409 workspace_busy`; `failed` answers `409 workspace_not_ready`; `archived` answers `409 workspace_archived`.
4. The workspace is a fork: `400 fork_has_no_origin`.
5. On a carry-patch workspace: the branch is the workspace's integration branch, `403 integration_branch`; the branch is the branch of a registered patch that is not deleted, `403 patch_branch`. Both hold with and without `force`. If the patch list cannot be read, the answer is `503 unavailable` and nothing is pushed.
6. `force` is true and the branch is protected: `403 protected_branch`. A branch is protected when at least one glob of the workspace's effective `protected_branches` matches its whole name: `*` matches any run of characters except `/`, `**` matches any run of characters including `/`, every other character matches itself (§8.3.1). A protected branch reaches origin only as a fast-forward.
7. The branch exists in the hub: else `404 branch_not_found`.

A request refused by any of these does not contact origin, changes nothing and writes no event.

### The push (WS-FR-049 to WS-FR-051, WS-FR-054)

The hub reads the head of `refs/heads/<name>` once; that commit is the `sha` of the answer and of the event, and it is what origin is offered. Exactly one ref is sent: no other branch and no tag of the hub appears on origin because of the call. The hub authenticates to origin with the workspace's origin credential, resolved from the workspace's secrets at the time of the push.

- **Origin's branch is already at that commit.** Nothing changes on origin. `result` is `up_to_date`, with and without `force`.
- **Without `force`.** The push is fast-forward only. It succeeds when origin has no such branch or origin's branch is an ancestor of the hub's; `result` is `pushed`. Otherwise origin is unchanged and the answer is `409 non_fast_forward`; the message names the origin head the hub last saw for the branch when it has one, and says to sync or to push with force.
- **With `force`.** The push is a force-with-lease. The lease is the origin head of that branch that the hub last saw, or "no such branch" when it never saw one. Origin's branch is replaced only if it still has that value. `result` is `forced` when the commit that was on origin is not an ancestor of the pushed one, and `pushed` when the push was a fast-forward or created the branch. If origin's branch is anywhere else — moved, created since, or deleted since — nothing is overwritten and the answer is `409 origin_moved`.

The push has a deadline of 2 minutes. It runs to completion or to its deadline when the caller disconnects, and the records below are written all the same.

The answer to a successful push is `200`:

```json
{"branch": "fix/42-nil-map", "sha": "<40 hex pushed>", "remote": "origin",
 "remote_url": "<git_url without userinfo>", "result": "pushed"}
```

### Failures of the push itself (§4.5)

| Status | `error_type` | Condition |
|---|---|---|
| `409` | `non_fast_forward` | Not forced, and origin's branch is not an ancestor of the hub's. |
| `409` | `origin_moved` | Forced, and origin's branch is not at the head the hub last saw. |
| `422` | `origin_rejected` | Origin refused the update, for example by a branch rule. The message carries origin's reason, scrubbed. |
| `502` | `origin_unauthenticated` | Origin wants authentication and the workspace has no credential, or origin rejected the stored one. The message says which. |
| `502` | `origin_forbidden` | The credential may not push to this repository. |
| `502` | `origin_not_found` | Origin has no repository at `git_url`, or the credential cannot see it. |
| `502` | `origin_unreachable` | DNS, connection or TLS failure. |
| `502` | `upstream_error` | Any other failure of the push. |
| `504` | `origin_timeout` | The push did not finish within 2 minutes. The `git` process is ended. |

After any of them origin's branch is where it was before the call, as far as the hub can tell, and the hub's record of the origin head is unchanged.

If the origin credential cannot be read from the workspace's secrets, the hub does not fall back to an anonymous push: the answer is `500 internal`, origin is not contacted and no event is written.

### Records after a push (WS-FR-055, WS-FR-056, §4.8, §14.8)

After a successful push (`pushed`, `forced` or `up_to_date`) the hub records the pushed commit as the origin head of that branch. For the default branch that value is the workspace's `origin_head_sha`.

Each push handed to origin writes exactly one `parley.workspace.push` event, on success and on each failure of the table above. The event has the workspace's slug as `workspace`, the caller as actor, `resource_type` `branch`, `resource_id` the branch name, and metadata `branch`, `sha`, `force`, `ok`, `remote_url` (without userinfo), and `result` when `ok` is true or `error_type` when it is false. On success the origin head and the event are written together.

### Scrubbing (WS-FR-057)

Every message of this route — every error message and every text in the event — is scrubbed before it leaves the hub: the value of the origin credential is replaced by `[redacted]`, userinfo is removed from URLs, and the text is cut to at most 1024 characters. `remote_url` is the workspace's `git_url` without userinfo.

### `parley workspace push <slug> <branch>` (§4.6)

- **Flags:** `--force`.
- **Calls:** `POST /workspaces/:slug/branches/:name/push` with the body `{"force": <bool>}`, the slug and the branch each sent as one percent-encoded path segment. The request timeout is 3 minutes, longer than the hub's 2-minute deadline, so the command receives the hub's answer, including `504 origin_timeout`.
- **Output:** the push answer on stdout, exit status 0. With `--force`, a warning on stderr, before the request is sent, that origin's branch may be replaced.
- **Errors:** a hub error is printed on stderr as received and the exit status is 1. Fewer or more than two arguments is a usage error: exit status 2, nothing sent.

### End-to-end flows

1. A developer runs `parley workspace push demo main` after work has landed on `main` in the hub. The CLI calls the route; `parleyd` checks the request, reads the head of `main`, pushes it to origin with the workspace's credential; origin fast-forwards `main`; `parleyd` records the origin head and the event and answers `200` with `result: "pushed"`; the CLI prints the answer and exits 0. `GET /workspaces/demo` then shows `origin_head_sha` equal to `head_sha`.
2. An agent rewrites a branch in the hub and calls the route with `{"force": true}` after someone else has pushed to the same branch on origin. `parleyd` offers the commit with the lease of the origin head it last saw; origin's branch is elsewhere, so nothing is overwritten; `parleyd` writes the event with `ok: false` and `error_type: "origin_moved"` and answers `409 origin_moved`. After a sync the same call answers `200` with `result: "forced"`.

## Design Decisions

1. **Language, git and tests (project-wide).** Go; the hub runs the `git` command (2.38 or later) as a subprocess through `internal/gitrepo`, with no git library; HTTP uses `net/http`; the CLI uses `github.com/spf13/cobra`; the store is SQLite behind `internal/store`; tests are table-driven with `testing` and `net/http/httptest`, and repositories are made with `git init` in `t.TempDir()`.
2. **The commit is pushed by id.** The hub reads the branch head once and pushes that commit to `refs/heads/<name>` on origin. A branch that moves in the hub while the push runs does not change what is pushed or what is recorded. No lock is taken (WS-FR-008).
3. **How the outcome is read.** One `git push` in machine-readable mode with a single refspec; a forced push passes the lease for that one ref explicitly (the recorded origin head, or "must not exist"). The per-ref status decides `pushed`, `forced`, `up_to_date`, `non_fast_forward` (git's non-fast-forward and fetch-first refusals), `origin_moved` (git's stale-lease refusal) and `origin_rejected` (any refusal origin itself reports for the ref). When no ref status is reported, the transport failure decides: HTTP 401 or a refused login is `origin_unauthenticated`, HTTP 403 `origin_forbidden`, HTTP 404 or "repository not found" `origin_not_found`, a name-resolution, connection or TLS failure `origin_unreachable`, anything else `upstream_error`. For a `git@host:path` origin the same classes are read from the SSH transport's messages.
4. **"Origin already had the commit" means origin's branch head equals it.** A hub branch that is strictly behind origin's is not up to date: without `force` it is `409 non_fast_forward`. The PRD's rule for a non-forced push ("origin's branch is an ancestor of the hub's") decides it.
5. **Origin's head in the `non_fast_forward` message is the recorded origin head.** The PRD says "when it is known"; the hub knows the head it last saw and makes no second call to origin to learn the current one. The message is for people and is not a contract.
6. **`up_to_date` is a successful push.** It records the origin head and writes an event with `ok: true`.
7. **Which requests count as handed to origin.** Every request that passes the seven checks and whose credential was resolved. A credential that cannot be read ends the request before origin is contacted: `500 internal`, no event. The PRD says only that the operation fails and never falls back to anonymous.
8. **A patch list that cannot be read answers `503 unavailable`.** The PRD names the two refusals only; CP-FR-083 asks for a retryable error and that nothing passes unchecked.
9. **How this package learns about patch branches and protected globs.** Patch branches: through the interface `PatchBranchLookup` that `internal/workspace` declares here, with `RegisterPatchBranchLookup`; package 31 implements and registers it, and with none registered no branch is a patch branch. Protected globs: the effective `protected_branches` setting, read with `EffectiveSettings` of package 10's settings registry; the key and its default (the default branch alone) are package 11's, which is a dependency. The integration branch is the workspace record's `integration_branch`; when that is `null`, no branch is refused as the integration branch.
10. **One branch-glob matcher, owned by package 11.** This package matches with `MatchAnyBranchGlob` of `internal/workspace/branchglob.go`, which package 11 delivers because it validates `protected_branches`. This package adds no matcher; packages 18 and 27 use the same function.
11. **`force: null` is `400`.** `null` is not a boolean.
12. **A fork answers `400 fork_has_no_origin` whatever the state of its parent.** The state guard is asked about the addressed workspace, and a fork is always `ready`; the write refusals of WS-FR-066 name git push, the source control API and update of the fork, not this route.
13. **No `Idempotency-Key`.** The route creates no resource; the header is ignored.
14. **The scrubbed limit.** The platform's scrubber (PLAT-FR-098) cuts to 1024 bytes on a character boundary, which is never more than the 1024 characters of WS-FR-057. This package uses that scrubber, with the resolved credential's value passed to it as a secret.
15. **Failure to write the records after origin accepted.** The answer is `500 internal`; origin keeps the commit, and the next sync or push records the origin head.
16. **The deadline is a parameter.** The handler is built with a push deadline whose production value is the fixed 2 minutes, so that tests can use a short one.
17. **Test origin.** A push needs git's smart HTTP protocol. The repository has one test origin helper, `internal/gitrepo/gitrepotest/`, owned by package 10; this package extends it and adds no second helper. It gains a mode (`SmartOrigin`) that serves a bare repository through `git http-backend` with pushes enabled, over TLS, with optional Basic authentication, and with switches to answer a fixed HTTP status, to refuse every update from a server-side hook with a given message, and to stall.
18. **The CLI always sends a body**, `{"force": false}` or `{"force": true}`, as the PRD's entry states.
19. **`remote_url` for a `git@host:path` origin is the stored value.** That form is not a URL and its `git@` is the form's fixed login, not a credential (as package 10 decided for the create event). For an `https` origin any userinfo is removed.
20. **Protected, integration and patch refusals do not depend on the branch existing.** The PRD's order puts "branch exists" last, so a forced push of a missing branch that a protected glob matches answers `403 protected_branch`.

21. **The recorded origin head is a ref.** The origin head of a branch is the ref `refs/remotes/origin/<name>` of the hub's copy, as package 12 keeps it and package 10's clone first writes it; this package reads it as the lease with `OriginHead` and sets it after a push with `SetOriginHead` of `internal/gitrepo`. A ref cannot be part of a store transaction, so the ref is set first and the transaction (the event, and `origin_head_sha` for the default branch) second; when the transaction fails the ref is put back.
22. **Composition.** The route is wired on the composition value of `internal/hub` (package 03).

## Dependencies

- **12 workspace_sync_from_origin** (draft): keeps the origin head of every branch current as `refs/remotes/origin/<name>`, which is the lease of a forced push and the way back from `origin_moved` and `non_fast_forward`; shares the origin transport of `internal/gitrepo`.
- **11 workspace_read_update_settings** (draft): the setting key `protected_branches` with its default, and the branch-glob matcher.
- Through them: 10 workspace_create_and_clone (workspace record, branch-name rule, state guard, origin credential resolution, origin heads, the settings registry, the test origin, `internal/cli/workspace.go`), 07 workspace_access_rule, 39 secrets, 52 durable_event_log_and_stream, 02 http_api_conventions, 03 server_lifecycle_and_configuration (the scrubber in `internal/scrub/`, the composition in `internal/hub/`), 09 cli_foundation.
- Not a dependency, used when present: 31 carry_patch_setup_and_patches (registers the patch-branch lookup). See Design Decisions, 9.
- External: the `git` command, 2.38 or later, on the hub's `PATH`.

## Verified External API

- `github.com/spf13/cobra` — **unverified** (not yet in `go.mod`): `cobra.Command` with `Use`, `Short`, `Args`, `RunE func(cmd *cobra.Command, args []string) error`; `cobra.ExactArgs(n int) cobra.PositionalArgs`; `(*Command).AddCommand(cmds ...*Command)`; `(*Command).Flags() *pflag.FlagSet`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token (§8). Nobody can force-push or delete it. By default the default branch.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Patch**: A carry-patch entry: a branch with a position in the patch order and a status.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Origin head**: For a branch of a repository workspace, the last head the hub saw on origin.
- **Lease**: The value a forced push requires origin's branch to still have: the recorded origin head, or "no such branch".
