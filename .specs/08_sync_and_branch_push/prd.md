---
spec_id: "08"
spec_name: "sync_and_branch_push"
title: "Sync a workspace with its remote and push its branches back"
status: "active"
created_at: "2026-10-27T18:42:04Z"
updated_at: "2026-10-28T09:39:48Z"
intent_hash: "7489002035f61aed999bd37ce77292846cc5b7d94d328f53a87fbe0b0ccdd0d1"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Sync a workspace with its remote and push its branches back

## Intent

A workspace that only ever receives pushes from agents drifts away from the
repository it came from, and a branch an agent builds inside Parley is
invisible to everyone until it reaches the forge. These two routes close that
loop in both directions: bring the remote's work in, and send a branch back
out. They are also the routes most likely to be run against a repository
someone else changed under them, so their refusals — diverged history, a
non-fast-forward push, a branch the hub must not send — matter more than
their successes.

## Goals

- `POST .../sync` on a standard workspace produces the six outcomes of
  `docs/prd/api.md` § POST .../sync, each with its status, its effect on
  `head_sha`, `upstream_head_sha`, `last_sync_at` and `sync_error`.
- `POST .../reclone` recovers a repository that is beyond repair without
  losing commits that exist only inside Parley.
- `POST .../branches/:name/push` performs the eight checks of its entry in
  that order, answers with the documented body and `result`, and finishes
  even if the caller goes away.
- No answer from either route carries a credential, the userinfo of a URL, or
  the underlying git error.
- `afc workspace sync`, `afc workspace reclone` and `afc workspace push`
  behave as `docs/prd/cli.md` specifies, exit codes included.

## Non-goals

- **The carry-patch sync.** `api.md` § Carry-Patch Sync Extension — fetching
  upstream, refreshing patch branches, detecting merges, queuing a rebuild
  and the `patches_synced` response — is `09_carry_patch`, which extends this
  package's sync route for `carry_patch` workspaces. This package implements
  the standard sync in full and refuses nothing on mode grounds.
- **Audit events.** `hub.workspace.sync` and `hub.branch.push` are
  `12_audit_and_events`.
- **Pushing patch branches to the fork from the receive path.** That is push
  control, already in `07_git_server`.
- **The forge's API.** This route pushes over git to the remote URL; reading
  or writing issues and pull requests is `13_forge_api`.

## Background

`05_repository_store` provides the copy-in and the credential resolution;
`07_git_server` provides the receive path. What is missing is the workspace's
own relationship with its remote over time.

Two details of `api.md` shape the work. The **sync's outcome table** is not
just success or failure: an origin that has diverged is `409` with a
`sync_error` that tells the caller to use `reset_to_upstream`, which is a
recovery instruction rather than an error, and a cancelled sync is `504` with
its own recorded error. A failed sync's `sync_error` is cleared by the next
successful one, so the field is a current state and not a log.

The **branch push** is the most precisely specified route in the reference. It
runs eight checks in a fixed order; it names the branch as one path segment
with each `/` escaped, decoded exactly once; its own error types are repeated
at the start of the message because `afc` prints only the message; it runs to
its end or its two-minute deadline even if the caller disconnects; and it
records every attempt but no refusal. Getting the order wrong produces the
right statuses for the wrong reasons, which no single test catches.

## Requirements

### Syncing a standard workspace

- A sync fetches `origin` with the workspace's stored credential and produces
  one of six outcomes: up to date, fast-forward, reset, diverged, fetch
  failure, or cancelled.
- Up to date and fast-forward answer `200`; a fast-forward advances
  `head_sha` and `upstream_head_sha` to origin's head. Both update
  `last_sync_at`.
- With `reset_to_upstream`, the workspace's branch is moved to origin's head
  whatever the ancestry, and `head_sha` and `upstream_head_sha` follow.
- A diverged origin — origin's head does not descend from the local head —
  answers `409`, and `sync_error` says to use `reset_to_upstream`.
- A fetch failure answers `502` and a sync cancelled mid-run answers `504`;
  both set `sync_error`.
- A failed sync's `sync_error` is cleared by the next successful one.

### When a sync may run

- The workspace must be `ready` and its `sync_enabled` setting true;
  otherwise the sync is `400`.
- A sync that finds another operation using the repository answers `409
  workspace_busy` and may be retried.
- A sync in progress is not a stored state: a second concurrent sync is
  refused by the lock, not by a status field.

### Recloning

- A reclone pushes what it can of the repository's branches to `origin` with
  the stored credential, then discards the repository, clears `sync_error`
  and `upstream_head_sha`, and copies the repository again. `status` returns
  to `pending` and follows the new copy.
- A failed push does not stop the reclone; the reclone goes on regardless.
- An archived workspace is `400`; a copy already under way is `409`; a held
  lock is `409 workspace_busy`; a repository that cannot be discarded is
  `500`.

### Pushing one branch to the remote

- The route sends exactly `refs/heads/<name>` to `refs/heads/<name>` on
  `origin`, with the workspace's stored credential, fast-forward only unless
  `force` is given.
- `:name` is one path segment with each `/` escaped, decoded exactly once, so
  `fix%2F42` names `fix/42` and `a%2525b` names `a%25b`. A name with an
  unescaped `/` matches no route.
- The checks run in this order: the credential; a PAT's `workspaces:write`,
  before the workspace is looked up; the workspace and write access; the
  body; the branch name against git's rules; `status` is `ready`; the
  branches this route will not push; the workspace lock without waiting;
  then, with `force`, the protected and missing-branch checks; then the push
  itself with a two-minute deadline.
- The branches this route will not push, the same whether the workspace is
  busy or not: a `carry_patch` workspace's integration branch, a registered
  patch that is not deleted, and — with `force` — the workspace's own branch.
- A successful push answers with the branch, the SHA offered, the remote, the
  remote URL without userinfo, and a `result` of `pushed`, `up_to_date` or
  `forced`.
- The push runs to its end or its deadline even if the caller goes away, and
  its outcome is recorded as if the caller were there.
- Every failure maps to one of the route's documented answers, and the
  message of an answer carrying one of this route's own error types begins
  with that type and `: `.
- No answer carries the credential, the userinfo of `git_url`, or the
  underlying git error.

### End-to-end flows

1. **A sync brings the remote's work in.** An API client syncs a standard
   workspace whose origin has advanced; Parley takes the lock, resolves the
   credential, fetches origin, fast-forwards the workspace's branch, advances
   `head_sha` and `upstream_head_sha`, sets `last_sync_at`, clears
   `sync_error` and answers `200` with the workspace.
2. **A force-pushed origin is recovered.** The same sync against an origin
   whose history was rewritten answers `409` with a `sync_error` telling the
   caller to reset; the client syncs again with `reset_to_upstream`, and the
   branch is moved to origin's head and the error cleared.
3. **A branch reaches the forge.** An agent pushes `fix/42-nil-map` to
   Parley's git server; the owner then calls the branch push route with the
   name as one escaped segment; Parley runs its checks, takes the lock,
   pushes the branch to origin with the stored credential and answers
   `result` `pushed` with the SHA it offered.
4. **A broken repository is recovered.** An API client reclones a workspace
   whose repository is unusable; Parley pushes what branches it can to
   origin, discards the repository, clears the sync fields and copies again,
   answering with `status` `pending`; the workspace reaches `ready`.

## Design Decisions

1. **The sync route is written for both modes from the start, and refuses
   neither.** This package implements the standard path completely and leaves
   a single documented branch point for `09_carry_patch` to extend, rather
   than having `09` rewrite the route. A `carry_patch` workspace synced
   before `09` exists is a gap in function, not a wrong answer.
2. **The order of the branch-push checks is a requirement of its own.** Each
   individual refusal can be made to pass with the checks in any order; only
   a test that varies two conditions at once distinguishes them, so the order
   is stated and tested directly.
3. **The push outlives its caller.** `api.md` requires it, and on a platform
   that cancels work when a client disconnects that has to be arranged
   deliberately rather than inherited.
4. **`sync_error` is a current state, not a history.** It is written on
   failure and cleared on the next success, which is what makes it safe for a
   client to treat as the workspace's present condition.

## Dependencies

- `01_service_foundation` for the router and the error envelope.
- `03_workspaces` for the workspace record, the access decision, the
  `sync_enabled` setting and the workspace lock.
- `05_repository_store` for the repository, the copy-in a reclone repeats and
  the credential resolution both routes use.
- `07_git_server` for the patch-branch identification the push route reuses
  when deciding which branches it will not send.
- `docs/prd/api.md` § POST .../sync, § POST .../reclone and
  § POST .../branches/:name/push are normative, and `docs/prd/cli.md`
  § afc workspace sync, reclone and push for what `afc` expects.

## Verified External API

Nothing was verified against installed code. Pushing to and fetching from a
third-party remote is performed through the git library chosen in
`06_history_engine` and verified there; this package adds no external
dependency of its own. The two-minute push deadline and the behaviour of a
request whose client has disconnected depend on the platform's own execution
model, which `01_service_foundation` establishes and this package's first
task confirms for a long-running outbound request.
