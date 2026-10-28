---
spec_id: "10"
spec_name: "merges"
title: "Merge agent branches into a target through a queue"
status: "active"
created_at: "2026-10-27T18:55:01Z"
updated_at: "2026-10-28T11:06:17Z"
intent_hash: "3e7df3fb4ac9c7955be85bbf3a418796d3e9fd6a1b30138042f0e18d489c310c"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Merge agent branches into a target through a queue

## Intent

When several agents work on one repository they all finish at once, and the
person who has to merge their branches by hand is the bottleneck the whole
service exists to remove. A merge job is Parley's answer: submit a branch,
and the service rebases it onto the target and fast-forwards the target,
one job at a time per workspace, with the outcome recorded whether it
succeeded or conflicted. Batch rebase is the same arithmetic offered
synchronously, for the case where a caller wants to know which of a set of
branches still apply.

## Goals

- A merge job behaves as `docs/prd/api.md` § Merge Endpoints specifies: the
  schema, the six statuses, the rebase-then-fast-forward semantics, and the
  refusal of a duplicate.
- Submit, list, read, cancel and requeue answer as their entries specify.
- Batch rebase runs while the request waits and reports one result per
  branch, a conflict on one not stopping the others.
- A job survives the restart of whatever runs it and never leaves the target
  branch half-written.
- `afc merge` and `afc rebase submit` behave as `docs/prd/cli.md` specifies.

## Non-goals

- **Computing the rebase.** `06_history_engine` does that; this package
  decides what to rebase onto what, in what order, and what to record.
- **Audit events.** `hub.merge.*` are `12_audit_and_events`.
- **Merging on the forge.** This merges inside the workspace's repository; a
  pull request on the forge is `13_forge_api`.
- **Rebuilds**, which are `09_carry_patch` and use the same engine for a
  different purpose.

## Background

Everything needed exists: the workspace and its lock (`03`), the repository
(`05`), the engine that rebases and reports conflicts (`06`).

Two details of `api.md` shape the work. A merge job **waits its turn** for
the workspace's lock rather than being refused — unlike a sync or an
archive, which answer `409 workspace_busy` at once. And the body of both
routes is **checked before the workspace**, so a malformed branch name is
`400` even for a slug the caller cannot see, which is the opposite of the
anti-enumeration rule elsewhere and is stated explicitly in the reference.

A merge is a rebase of the source onto the target followed by a
fast-forward of the target, so the resulting history is the source's commits
on top of the target's, with no merge commit. That is the same operation
`06_history_engine` proves against the real `git` binary.

## Requirements

### A merge job

- A job rebases its source onto its target and fast-forwards the target to
  the result, inside the workspace's repository.
- It moves through queued, running, and then completed, failed, dead_letter
  or cancelled, recording the target's head before and after, the
  conflicting files, the error, the retry count and who submitted it.
- A job waits its turn for the workspace's lock rather than being refused
  for a busy workspace.
- The history a completed job produces is the history the real `git` binary
  produces for the same rebase, which `06_history_engine` guarantees.

### Submitting, following and undoing a job

- A submit names a target branch and a source ref, both required, both
  following git's branch-name rules and both existing in the workspace. The
  body is checked before the workspace.
- A submit answers `202` with the queued job; a malformed body, an invalid
  or missing name, a workspace that is not ready, or a branch that does not
  exist is `400`; a job for the same source and target already queued or
  running is `409 duplicate_merge`.
- A workspace's jobs are listed, at most 200, and one is read.
- A queued job is cancelled; any other status is `409`. A dead-lettered job
  is requeued; any other status, or a duplicate of one already queued or
  running, is `409`.

### Batch rebase

- A batch rebase rebases each branch onto the target in turn while the
  request waits, and answers with one result per branch: ok with the new
  head, conflict with the conflicting files, or error.
- A conflict on one branch does not stop the others.
- A malformed body, a missing target, an empty branch list, an invalid name
  or a workspace that is not ready is `400`, naming the offending branch by
  its index where there is one. The body is checked before the workspace.
- A held lock is `409 workspace_busy`; a failure of the rebase itself is
  `500`. At most 20 branches are accepted in one request.

### Durability

- A job survives the restart or eviction of whatever runs it: it is either
  completed or returned to queued, never lost.
- A job that makes no progress for 15 minutes becomes dead-lettered and can
  be requeued.
- The target branch is either at the merged result or at the head it had
  before the job; it is never left part way.

### End-to-end flows

1. **An agent's branch is merged.** An API client submits a merge of
   `feature/agent-1` into `main`; Parley queues the job and answers `202`.
   The job takes its turn for the lock, rebases the source onto the target
   through the history engine, fast-forwards the target, records the heads
   before and after and completes. The client polls it to completed.
2. **A conflicting merge is recorded.** The same submit for a branch that
   conflicts; the job runs, the engine reports the conflicting files, the
   job ends failed with those files recorded, and the target branch is where
   it was.
3. **Several branches are rebased at once.** An API client submits a batch
   rebase of three branches onto `main`; Parley rebases each in turn while
   the request waits and answers with three results, one of them a conflict
   and the other two new heads.

## Design Decisions

1. **A merge job waits for the lock; a batch rebase does not.** The
   reference distinguishes them, and the distinction is deliberate: a queued
   job has somewhere to wait, and a synchronous request does not.
2. **The body is validated before the workspace is looked up**, as the
   reference states for both routes. It is a deliberate exception to the
   anti-enumeration rule and is written as a requirement so that a later
   reader does not "fix" it.
3. **A merge is a rebase and a fast-forward, not a merge commit.** The
   reference is explicit, and the resulting history is therefore the one the
   differential suite of `06_history_engine` already proves.

## Dependencies

- `03_workspaces` for the workspace, its access decision and its lock.
- `05_repository_store` for the repository the merge happens in.
- `06_history_engine` for the rebase, the conflict detection and the
  guarantee that the result matches the real `git` binary.
- `docs/prd/api.md` § Merge Endpoints is normative, and `docs/prd/cli.md`
  § Merge Commands and § Rebase Commands for what `afc` expects.

## Verified External API

Nothing new. The git arithmetic is verified in `06_history_engine` and the
queue is a platform interface following the pattern of
`01_service_foundation`.
