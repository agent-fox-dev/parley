---
spec_id: "05"
spec_name: "repository_store"
title: "Hold each workspace's repository in Cloudflare Artifacts"
status: "active"
created_at: "2026-10-27T18:26:21Z"
updated_at: "2026-10-28T06:17:30Z"
intent_hash: "4714795fa465485eed31dddff33212ff5963250db0132806b594e5cfb5035334"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Hold each workspace's repository in Cloudflare Artifacts

## Intent

A workspace without a repository is a row in a table. This is where Parley
starts holding real git history: one durable repository per workspace, copied
in from the forge with the workspace's own credential, and reachable by every
later operation through one interface rather than through a vendor's SDK
scattered across the codebase. It is also where the port's largest unknowns
are settled — what the store will and will not do with refs — because every
package after this one is built on the answer.

## Goals

- Every workspace has exactly one durable repository, created when the
  workspace is created and surviving the restart of every other component.
- A **private** repository is copied in with the workspace's stored
  credential. This is the ordinary case, not an edge case.
- The workspace's status moves `pending` → `cloning` → `ready` or `failed`
  as `docs/prd/api.md` describes, with `clone_error` set on failure and
  `head_sha` set on success, and never stalls.
- The git credential of a workspace is resolved from the reserved secret
  names, in the documented order, for `origin` and for `upstream`.
- What the store does with ref namespaces outside branches and tags, with a
  force update and with a ref deletion is **established by exercising it**,
  and recorded, before any later package depends on it.
- The repository store is a platform interface with two implementations and
  one contract suite, as `01_service_foundation` established.

## Non-goals

- **Serving git to clients.** The smart HTTP routes, their authentication,
  push control and the protected namespace are `07_git_server`.
- **Computing history.** Rebase, merge strategies and rerere are
  `06_history_engine`; this package stores and transfers objects and refs and
  computes nothing.
- **Sync, reclone and branch push.** `08_sync_and_branch_push` adds those;
  this package provides the copy-in they are built from and the credential
  resolution they use.
- **`refs/hub/replaced/` as a feature.** This package establishes whether the
  store accepts such refs and records the answer. Keeping replaced patch tips
  is `09_carry_patch`, which chooses its mechanism from what is found here.
- **Audit events** (`hub.workspace.*`), which are `12_audit_and_events`.

## Background

`03_workspaces` creates a workspace, validates the body, stores the creation
credentials and answers `201` with `status` `pending` — and stops. `api.md`
has the create route also check those credentials against the repository
before the workspace exists, and then start the clone. This package completes
that route.

**The store is Cloudflare Artifacts** (decision D2 of the source PRD), read
from the installed binding types rather than from the documentation:

- The Worker binding offers `create(name, opts?)`, `get(name)`, `list(opts?)`,
  `import(params)` and `delete(name)`, and a repository handle offers
  `info()`, `createToken(scope?, ttl?)`, `listTokens()`, `revokeToken()`,
  `fork()`, `log()`, `readCommit()`, `readTree()`, `readBlob()` and
  `readFile()`.
- There is **no object-level write API**: everything is written by pushing
  over the git protocol, to the repository's `remote` URL with a token the
  deployment mints.
- There is **no ref listing**: nothing enumerates branches or tags, and every
  reader needs a ref or a hash the caller already holds. What a repository
  contains is answered over the git protocol.
- There is **no storage error code**: nothing the store reports names the
  1 GB cap, so a refusal that names a limit is Parley's own.
- A repository name may hold only alphanumerics, dots, hyphens and
  underscores — no slashes, so no `org/workspace`.
- `import()` takes a **public** HTTPS URL and no credential, and runs once,
  with a `REMOTE_AUTH_REQUIRED` error for a remote that needs one.
- Storage is capped at **1 GB** for a repository created on or after 29
  September 2026.
- Tokens are repo-scoped `read` or `write`, with a TTL from 60 seconds to a
  year.

Three capabilities the contract needs are **not documented**: ref namespaces
beyond branches, tags and `refs/notes/*`; force push; and ref deletion. All
three are needed — mirroring patch branches force-pushes, the integration
branch is force-updated, branches get deleted, and `refs/hub/replaced/` is a
namespace of its own. Decision D16 of the source PRD says the first package
touching the store establishes what it really accepts, as its first task.
This is that package.

How the store is reached, and what the undocumented capabilities mean for the
design, is [ADR 2](../../docs/adr/02-store-repositories-as-refs-and-packs-over-our-own-git-client.md).

Because `import()` cannot carry a credential and cannot be repeated, Parley
copies every repository in itself, by the same path every later sync uses
(decision D15). One path, always exercised, identical for a public and a
private remote.

## Requirements

### One repository per workspace

- Every workspace has exactly one repository in the store, named
  deterministically from the workspace so that the mapping needs no separate
  record to be trusted.
- Creating a workspace creates its repository; the repository outlives the
  restart or eviction of every other component.
- Two operations that write one workspace's repository never run at the same
  time; they take the workspace lock.

### Copying a repository in

- Creating a workspace copies the repository at `git_url` into the
  workspace's store, with the workspace's stored credential where it has one
  and without one where the remote is public, bringing every branch and tag.
- The credentials given on create are checked against the remote **before**
  the workspace is created; credentials the remote refuses are `400` and no
  workspace is created. Only an authentication refusal stops the create: a
  remote that cannot be reached, or that answers with a server failure or not
  at all, answers `201` and fails the copy afterwards.
- A copy that fails for any other reason — the network, the remote's refusal
  after creation, a repository that does not exist — leaves the workspace at
  `status` `failed` with `clone_error` set, readable and recoverable.
- Work that cannot finish in one unit of platform work continues in another
  and finishes, resuming at the first ref it has not yet brought across
  rather than starting again.

### Status

- `status` moves `pending` → `cloning` → `ready` or `failed`, and `head_sha`
  carries the repository's `HEAD` once it is `ready`.
- A workspace does not stay in `cloning` for longer than 30 minutes; past
  that it becomes `failed` with `clone_error` set, so no workspace is stuck.
- Reactivating an archived workspace whose repository was kept reuses it
  rather than copying again.

### What the store really does

- The repository store reports which of three capabilities it has: refs
  outside `refs/heads/` and `refs/tags/`, a non-fast-forward update, and a
  ref deletion. Each implementation declares what it has, and the declaration
  is held against a real store by exercising all three, so a report that does
  not match the store is a failure rather than a belief.
- An operation that needs a capability the store lacks fails with an answer
  that names the capability, and never silently does something else instead.

### Size

- A repository of at most 500 MB of packed objects and at most 10,000 refs
  is copied, read and written successfully.
- An operation that would take a repository past what the store accepts fails
  with the route's documented storage-failure answer, naming the limit it
  exceeded, and leaves the workspace usable. Because the store reports no
  storage error of its own, the service accounts for what it writes and names
  the 1 GB repository cap itself.
- The 500 MB and 10,000-ref figures are provisional until measured against a
  real store; the fixtures that reach them are generated rather than
  committed, and skip with a named reason where they cannot be built.

### Resolving a workspace's git credential

- The `origin` credential is `GIT_PAT`, else `GIT_USERNAME` with
  `GIT_PASSWORD`, else none.
- The `upstream` credential is `UPSTREAM_GIT_PAT`, else
  `UPSTREAM_GIT_USERNAME` with `UPSTREAM_GIT_PASSWORD`, else the `origin`
  credential.
- A username without its password, or a password without its username, is
  ignored and counts as no credential; the service records that it was
  ignored.
- Only the **workspace** scope is read: a secret of the same name at an
  organization or a user scope is never used.

### Retiring a repository

- Archiving a `ready` workspace first pushes every branch of its repository
  to `origin` with the stored credential, then releases the repository. The
  push does not force: a branch `origin` refuses is a failed push, not a
  rewritten history.
- If that push fails, or there is no usable credential, the workspace is
  archived and the repository is **kept**, so no commit is lost and
  reactivation reuses it.
- Deleting a workspace removes its repository from the store.

### The interface

- The repository store is a platform interface with a Cloudflare
  implementation over Artifacts and a non-Cloudflare one, and one contract
  suite that passes against both. Its vocabulary is refs and packs — list
  refs, update refs, receive a pack, serve one, read an object — and not the
  shape of any one store's SDK.
- Parley speaks the git protocol itself, over smart HTTP, to the forge and to
  the store alike. Nothing shells out to `git`.
- No store credential — an Artifacts repo-scoped token among them — reaches
  a client, a response body or a log record.

### End-to-end flows

1. **A private repository is registered.** An API client creates a workspace
   with a `git_pat` for a private repository; Parley checks the credential
   against the remote, stores it, writes the workspace at `pending` and
   answers `201`; the copy then runs, the repository lands in the store with
   every branch, `status` becomes `ready` and `head_sha` is set; the client
   polls the workspace and sees it ready.
2. **A copy fails and the workspace says why.** An API client creates a
   workspace for a remote that is unreachable; the create succeeds, the copy
   fails, and the workspace is left at `failed` with `clone_error` naming the
   failure. The client reads the workspace and sees both.
3. **A repository is retired.** The owner archives a `ready` workspace;
   Parley pushes its branches to `origin` with the stored credential and then
   releases the repository. The owner later deletes the workspace and the
   repository is removed from the store.

## Design Decisions

1. **Capability discovery is task one, not a later surprise.** Three
   capabilities the contract needs are undocumented. The first task of this
   package exercises each against a real store and records the result in an
   ADR, because `07`, `08` and `09` are all designed differently depending on
   the answer, and finding out at package nine would invalidate three
   packages of work.
2. **Parley copies every repository in; `import()` is not used.** It takes no
   credential and runs once, so it cannot serve a private remote or any later
   sync. One path, exercised on every workspace, is worth more than a faster
   path for the public case that is rarely run and therefore rarely right.
3. **The repository name is derived from the workspace, not stored.** A
   deterministic name means the mapping cannot drift from the workspace
   record, and a repository orphaned by a failed delete can still be found.
4. **The credential resolution lives here, next to its first caller.**
   `04_secrets_and_variables` deliberately treats the reserved names as
   ordinary secrets; this package is where they first mean something.
5. **The interface speaks refs and packs.** The store offers no write API and
   no ref listing, so an interface shaped like its SDK would leave every
   write outside the abstraction. Refs and packs is the vocabulary both
   implementations can honour, and the one `07_git_server` relays through.
6. **Parley implements git smart HTTP v2 itself.** A copy asks for a
   self-contained pack and relays it into the store, decoding almost nothing
   and streaming rather than buffering, which is what makes a copy of the
   stated size conceivable inside one Worker. Object-level work, and the
   library that does it, belong to `06_history_engine`.
7. **A copy is checkpointed by ref batch.** Git cannot resume a pack, so the
   grain of recovery is the ref: the default branch first, then batches that
   ask only for what the earlier ones did not bring, with the cursor in the
   job record. This package therefore introduces the job-runner platform
   interface, which `01_service_foundation` did not define.
8. **The capability report is declared and probe-verified, not probed at
   runtime.** Probing on a cold path costs a scratch repository and caches
   the wrong answer when the store is briefly unwell. A declaration that a
   test holds against the real store gives the same certainty and no runtime
   cost.
9. **The capability probe runs before `02` to `04` are implemented.** It
   needs neither, and `07`, `08` and `09` are designed on its answer.
10. **Only an authentication refusal stops the create.** A forge that is
    briefly down is not a bad request, so everything but `401` and `403`
    answers `201` and reaches the client through `clone_error`.
11. **The service accounts for the bytes it writes.** No error code names the
    1 GB cap, so naming it is the service's own job.
12. **The archive push does not force.** Pushing out a workspace's branches
    may not rewrite history on someone else's forge; a refused branch keeps
    the repository instead, which loses nothing.

Decisions 5 to 12 are recorded as
[ADR 2](../../docs/adr/02-store-repositories-as-refs-and-packs-over-our-own-git-client.md).

## Dependencies

- `01_service_foundation` for the platform interface pattern and the contract
  suite this package adds to.
- `03_workspaces` for the workspace record, the create route this package
  completes, the workspace lock, and the secret store the credentials were
  written through.
- `04_secrets_and_variables` for the entry store this package reads the
  reserved workspace secrets from.
- **Cloudflare Artifacts** — open beta, Workers Paid, 1 GB per repository,
  three needed capabilities undocumented. The port's largest external risk,
  and the reason for the platform interface.
- `docs/prd/api.md` § Workspace Endpoints, § Workspace Response Schema and
  `docs/prd/cli.md` § Reserved secret names are normative.

## Verified External API

The Cloudflare Artifacts binding was **verified against the installed types**
(`@cloudflare/workers-types`, experimental entry point), and the source PRD's
reading of the documentation holds: `import()` takes
`{source: {url, branch?, depth?}, target: {name, opts?}}` with no credential
field, and the repository handle has no write surface — everything is written
over the git protocol.

Three things the documentation did not say, and the types do:

- the handle offers no ref listing at all, so the contents of a repository are
  read over the git protocol and not through the binding;
- the error codes (`ALREADY_EXISTS`, `NOT_FOUND`, `CREATE_IN_PROGRESS`,
  `IMPORT_IN_PROGRESS`, `FORK_IN_PROGRESS`, `INVALID_INPUT`,
  `INVALID_REPO_NAME`, `INVALID_TTL`, `INVALID_URL`, `REMOTE_AUTH_REQUIRED`,
  `UPSTREAM_UNAVAILABLE`, `MEMORY_LIMIT`, `INTERNAL_ERROR`) include none for
  exhausted storage;
- a repository name may hold only alphanumerics, dots, hyphens and
  underscores, and a handle is a disposable RPC stub.

**NOT FOUND** still, and established by this package's first task rather than
assumed: ref namespaces beyond branches, tags and `refs/notes/*`; force push;
ref deletion; and the per-namespace request rate limit. No git library is
used to speak the protocol: the client is Parley's own (ADR 2).
