---
spec_id: "07"
spec_name: "git_server"
title: "Serve each workspace's repository over git smart HTTP"
status: "active"
created_at: "2026-10-27T18:36:40Z"
updated_at: "2026-10-28T08:55:37Z"
intent_hash: "8e45a6a26ca6986c338ac597ae034541f730504d27dc65f43bbb5f2fa065bc11"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Serve each workspace's repository over git smart HTTP

## Intent

An agent does not call an API to get code; it runs `git clone`. This is the
surface that makes Parley usable at all: the workspace's repository, served
at the hub's own address, authenticated with the hub's own credentials, and
governed by the hub's own rules about who may push what. The store speaks
git too, but it authenticates with its own tokens and enforces none of the
hub's rules, so Parley stands in front of it rather than handing clients its
address.

## Goals

- `git clone`, `git fetch` and `git push` work against
  `<external_url>/git/<org>/<slug>.git` with an API key, a PAT or an admin
  token as the HTTP Basic password, exactly as `docs/prd/api.md` § Git Server
  Endpoints specifies.
- The access table holds: the owner and an admin token fetch and push, a
  member fetches and is refused a push, everyone else gets `repository not
  found`, and every refusal is a git pkt-line error the user sees.
- Push control on a `carry_patch` workspace produces the four outcomes of the
  § Pre-Receive Hook table, per ref, although the store offers no pre-receive
  hook.
- No client push can touch `refs/hub/replaced/`, on any workspace with any
  credential, while fetching those refs works normally.
- `afc credential-helper` plus a plain `git clone` of `hub_url` works, which
  is the Quick Tour of `docs/prd/cli.md`.

## Non-goals

- **Queuing rebuilds after a push.** `api.md` has an accepted update of a
  registered patch branch queue a rebuild unless `auto_rebuild_after_push` is
  false. Rebuilds are `09_carry_patch`, which adds that to this package's
  after-push step, as `05` completed `03`'s create route.
- **Audit events.** `hub.git.push` is `12_audit_and_events`.
- **Computing history.** Nothing here rebases or merges; `06_history_engine`
  does that and `10_merges` drives it.
- **Pushing to the forge on a branch route.** `POST .../branches/:name/push`
  is `08_sync_and_branch_push`. This package forwards and mirrors patch
  branches only because push control requires it in the receive path.

## Background

`05_repository_store` holds each workspace's repository in Cloudflare
Artifacts and establishes what the store will accept. Artifacts speaks the
full git smart HTTP protocol itself, but it authenticates with repo-scoped
tokens of its own and enforces nothing about owners, members or patch
branches. The contract fixes all of that under the hub's address, so Parley
serves the routes and relays to the store with a token it mints and never
discloses (decision D13 of the source PRD).

The hard part is push control. `api.md` describes it as a pre-receive hook,
and the store has none: its only push signal is an event that fires
afterwards. So Parley has to decide each ref of a push **before** any of it
reaches the store, and still produce git's per-ref report — because the
contract says a push whose refs are partly refused applies the accepted ones
and reports each refused ref on its own. The same mechanism carries the
protection of `refs/hub/replaced/`, where the contract is precise: the push
reports `unpack ok`, the protected ref is reported `ng` with a fixed message,
the other refs are processed as usual, and the ref keeps its old value.

Whether the store accepts a ref outside branches and tags at all is one of
the three capabilities `05` establishes by experiment. If it does not,
fetching `refs/hub/replaced/<branch>` cannot work, which decision D16 of the
source PRD already records as the one documented operation the port may lose.
The refusal of a *push* to that namespace does not depend on the store, and
holds either way.

## Requirements

### Where the repository is served

- Each workspace's repository is served at
  `<external_url>/git/<org-slug>/<workspace-slug>.git`. The `.git` suffix is
  required and a URL without it is not found.
- A workspace with no organization has no URL, and its `hub_url` is null.
- The `hub_url` a workspace reports is the address this package actually
  serves.

### Authenticating a git client

- HTTP Basic authentication: the username is ignored and the password is a
  hub credential — an API key, a PAT or an admin token.
- A missing or invalid credential is refused, and so is a credential whose
  user is blocked.
- Every refusal is a git pkt-line error, so `git` shows it to the person
  rather than failing opaquely.
- No store credential is ever accepted from a client or disclosed to one.

### Who may fetch and who may push

- The workspace's owner and an admin token may fetch and push. A member of
  the workspace's organization may fetch, with the owner's result, and is
  refused a push. Everyone else is told the repository is not found, for
  fetch and push alike.
- The checks run in this order: the workspace exists and matches the
  organization in the URL, its repository is ready, then access, then a PAT's
  git scope. A member's push is therefore refused for being a member whatever
  the PAT's scopes.
- A failed access lookup is a pkt-line server error, never a grant.
- A PAT bound to another workspace is told the repository is not found.

### Fetching

- The ref advertisement serves both services, and a missing or invalid
  `service` parameter is refused with the documented message.
- A fetch into a repository that already holds part of the history transfers
  only the objects it lacks.
- Both protocol versions the store speaks are served, whichever the client
  asks for, and a gzip-encoded request body is accepted.

### Pushing

- The ref advertisement for the receive service needs write access, as the
  push itself does.
- A push applies the refs it accepts and answers with git's per-ref report.
- A push refused outright changes nothing in the repository.

### Push control on a carry-patch workspace

A **registered patch branch** is a `refs/heads/<name>` whose `<name>` is a
patch of the workspace that is not deleted and is not the integration
branch. Every other update — other branches, tags, and every ref on a
standard workspace — is unaffected. What happens to an update of a
registered patch branch depends on two settings:

- `patch_branch_source` `origin` with `push_patches_to_origin` false:
  **refused**, with the message `branch is synced from origin; push to
  <git_url> instead`, and `branch is synced from origin; delete it on the
  fork instead` for a deletion.
- `patch_branch_source` `origin` with `push_patches_to_origin` true:
  **forwarded** to the fork first, not forced, and accepted only if the fork
  accepts it. A failure is reported as `origin rejected push: <error>` or as
  a failure to resolve the fork's credentials. A deletion is refused as
  above.
- `patch_branch_source` `hub` with `push_patches_to_origin` true:
  **accepted**, then force-pushed to the fork on a best-effort basis; a
  failed mirror fails neither the push nor anything downstream.
- `patch_branch_source` `hub` with `push_patches_to_origin` false:
  **accepted**.

A refused or failed update is reported for that ref alone; the other refs of
the same push are processed on their own.

### The protected namespace

- No client push may create, update or delete a ref under
  `refs/hub/replaced/`, on any workspace and with any credential. The push
  reports `unpack ok`, the protected ref is reported `ng <ref>
  refs/hub/replaced/ is maintained by the hub and cannot be pushed to`, the
  other refs are processed as usual, and the ref keeps its old value.
- Fetching those refs works normally where the store allows the namespace at
  all.

### After a push

- The workspace's `head_sha` is refreshed from the repository's `HEAD` after
  every push, including one whose every ref was refused.
- Only the refs the push accepted have any effect.

### End-to-end flows

1. **An agent clones and pushes.** A git client configured with the
   credential helper clones `hub_url`; Parley authenticates the API key,
   admits the owner, mints a read token for the workspace's repository and
   relays the fetch, and the client receives a full pack. The client commits,
   pushes a new branch, and Parley admits the push, relays it to the store
   and refreshes the workspace's `head_sha`.
2. **A member clones but cannot push.** A member of the workspace's
   organization clones successfully and then pushes; Parley refuses with the
   pkt-line `403 ERR org members have read-only access to this workspace`,
   and the repository is unchanged.
3. **A patch branch is refused while the rest of the push lands.** On a
   `carry_patch` workspace with `patch_branch_source` `origin` and
   `push_patches_to_origin` false, a client pushes two refs in one push: a
   registered patch branch and an ordinary branch. Parley refuses the patch
   branch with the documented message and accepts the ordinary branch, and
   `git push` reports each outcome against its own ref.
4. **The protected namespace holds.** A client pushes an update to
   `refs/hub/replaced/<branch>` together with an ordinary branch. The push
   reports `unpack ok`, the protected ref is reported `ng` with the fixed
   message and keeps its old value, and the ordinary branch is accepted.

## Design Decisions

1. **Push control is enforced in front of the store, not by a hook.** The
   store has none. Parley reads the push's ref commands before any of it is
   relayed, drops the refs it refuses, relays the rest, and merges the
   store's report with its own refusals so the client sees one per-ref report.
   The objects of a dropped ref may travel with the pack; git tolerates
   objects no accepted command references.
2. **The hub's address is the only one clients get.** A store token is minted
   per request for the relay, is never returned and is never accepted from a
   client. Handing out a store remote would bypass the access rules, push
   control and the protected namespace all at once.
3. **Both protocol versions are served**, which is decision D3 of the source
   PRD and a recorded divergence from `api.md`: the reference describes the
   hub as speaking v0 with v2 falling back, and the store already speaks
   both.
4. **Mirroring and forwarding live here**, with push control, because they
   happen in the receive path and are part of the decision about a ref.
   Queuing a rebuild does not, and is left to `09_carry_patch`.

## Dependencies

- `01_service_foundation` for the router and the service's address.
- `02_identity_and_credentials` for the credentials a client authenticates
  with and the git scopes a PAT is checked against.
- `03_workspaces` for the access decision, the workspace settings that govern
  push control, and the bound-token confinement.
- `05_repository_store` for the repository, the relay, the store's capability
  report and the credential resolution the forward and the mirror use.
- `docs/prd/api.md` § Git Server Endpoints is normative, and
  `docs/prd/cli.md` § Git Credential Helper and § Quick Tour for what a
  client does.

## Verified External API

Nothing was verified against installed code. This package needs to parse and
produce git's pkt-line framing — the ref advertisement, the receive-pack
command list and the per-ref report — in order to decide a push before
relaying it. Whether `isomorphic-git` exposes that framing, or whether it
must be implemented directly from the protocol documentation, is established
by this package's first task and recorded. The store's protocol support and
its capability report come from `05_repository_store`, which verifies them
against the installed binding.
