---
spec_id: "13"
spec_name: "forge_api"
title: "Read and change a workspace's repository on its forge"
status: "active"
created_at: "2026-10-27T19:08:11Z"
updated_at: "2026-10-28T13:34:54Z"
intent_hash: "6a07fdea6ce542a4259d66c3e8fbefe0f9ab9eef122b7792f6118eae98c2ed7d"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Read and change a workspace's repository on its forge

## Intent

An agent that can change code but cannot open the pull request, read the
issue it was given or leave a comment is only half useful — and handing it a
GitHub token so it can do those things itself is exactly the credential
sprawl the service exists to prevent. These routes let an agent work on the
forge through Parley, with the credential the workspace already holds, so
the token stays in one place and every write is attributable.

## Goals

- The nine read routes and the nine write routes of `docs/prd/api.md`
  § Forge Endpoints answer with the documented types and statuses, for
  GitHub and GitLab.
- The hub reaches the forge with the **workspace's** credential, never the
  caller's, and no response shows the credential or any URL's userinfo.
- `?target=upstream` works on a `carry_patch` workspace, with the upstream
  credential and the upstream API URL setting.
- Every failure maps to one row of the § Failures of the forge table, with
  the forge's message scrubbed and truncated.
- `afc forge status`, `afc issue`, `afc pr` and `afc label` behave as
  `docs/prd/cli.md` specifies.

## Non-goals

- **Webhooks.** Setting the secret, the delivery log and the ingress are
  `14_forge_webhooks`.
- **Pushing code.** Getting a branch to the forge is
  `08_sync_and_branch_push`; this package opens the pull request for a
  branch that is already there.
- **Audit events.** `hub.forge.*` write events are recorded by the recorder
  `12_audit_and_events` built; this package calls it at the documented
  places.
- **Caching.** The reference is explicit that nothing is cached, so a
  rotated secret or a changed setting applies to the next call.

## Background

Everything needed exists: the workspace and its settings (`03`), the
credential resolution for origin and upstream (`05`), and the event recorder
(`12`).

Three parts of `api.md` carry most of the difficulty. **Working out which
forge** is a small decision procedure with several inputs — the remote URL's
host, the `forge_api_url` setting, and, for reads only, the deployment's own
environment — whose failure is `422 forge_unknown` rather than a guess.
**The order of checks** differs between reads and writes and is given
step by step, including that a body-like field over the forge's limit is
checked *after* the forge is identified, so a forge the hub cannot tell is
`422` even for an oversized body. And the **failure table** maps fifteen
conditions onto statuses that are deliberately not the forge's own: a
forge's `401` becomes `502 forge_unauthenticated`, because the caller's
credential was fine and the workspace's was not.

Two behaviours are easy to implement as "whatever the forge did". A label
create answers `200` for a label that already exists and never changes it,
re-reading once if the create is refused by a race. A label removal answers
`204` even when the issue never had the label, re-reading the issue once if
the forge answers not found, and only then answering `404`.

## Requirements

### Reaching the forge

- The repository is read from the remote URL — `https://`, `ssh://` or
  scp-style — with `.git` and any userinfo removed, keeping a GitLab
  subgroup path as the owner.
- The forge is GitHub or GitLab, told from the host's name or from an API
  URL setting containing one of the two words. Without a setting the API URL
  is the host's documented default. When neither the host nor a setting
  tells the forge, the answer is `422 forge_unknown` and nothing is sent.
- The credential is the workspace's: a token, a username and password whose
  password is sent as the token, or none. The caller's own credential and
  the hub's own environment tokens are never sent.
- `?target=upstream` uses the `upstream_url`, the upstream credential and
  the `upstream_forge_api_url` setting, which does not fall back to
  `forge_api_url`. Any value other than `origin` and `upstream` is `400`.
- Nothing is cached: a rotated secret or a changed setting applies to the
  next call.
- Every request to the forge carries the hub's user agent, refuses a
  redirect to another host, port or scheme, and has 30 seconds including
  retries.

### Describing the forge

- `GET .../forge` reports the forge, the API URL, the repository, the
  credential's kind, the upstream's equivalent, and the webhook block —
  without contacting the forge.
- The webhook block says whether the workspace has the secret, where the
  forge should post, and when the last delivery arrived; it never carries
  the secret.
- An invalid API URL setting is `422 forge_api_url_invalid`, naming the
  setting and the rule but not the value; a store failure is `500` with no
  partial answer.

### Reading from the forge

- The nine read routes return the repository, issues, an issue thread,
  comments, a pull request, its state, its files, its checks and its
  reviews, each in the documented shape, a list always as a bare array.
- The issue list's parameters are validated and sent only when given, so the
  forge's own defaults apply otherwise. A bad, empty or repeated value is
  `400` with a message beginning with the parameter's name.
- An issue thread says whether more comments exist than were read, and
  carries the comments' error with a `200` when the issue was read but its
  comments were not.
- A path number that is not a positive decimal integer is `400`.

### Writing to the forge

- The nine write routes create and update issues, close them with an
  optional comment, comment on issues and pull requests, add and remove
  labels, ensure a label exists, and open pull requests.
- An issue update changes only the fields given; `labels` and `assignees`
  replace the whole set; a body that sets nothing is `400 nothing to
  update`, and so on GitLab is one that sets only assignees.
- A close with a non-blank comment posts the comment first and closes only
  if that worked; a refused comment leaves the issue open, and a close that
  fails after the comment still reports the comment's URL in its event.
- A label removal answers `204` even when the issue never had the label,
  re-reading the issue once when the forge answers not found and answering
  `404 forge_not_found` only if the issue is gone.
- A label create answers `200` for a label that exists, never changing it,
  and re-reads once if its create is refused.
- A pull request's head is sent as given, so a fork's `owner:branch` works;
  the hub keeps no idempotency key, so a retried create can create twice.
- Writes need a workspace that is not archived. A write that reached the
  forge records one event, successful or not; one the hub refused itself
  records none.

### Body limits

- An issue's body on create and update, a close's comment, a comment's body
  and a pull request's body are measured in Unicode code points against the
  forge's limit, and one over it is `413 body_too_large` with nothing sent.
- Titles, heads, bases, label names and descriptions and assignee names are
  not measured.

### Failing

- Every failure maps to one row of the § Failures of the forge table, with
  the documented status and error type.
- The forge's own message is carried through with the token replaced, URL
  userinfo removed and the text cut to 1024 bytes. A `401` for a username
  and password adds the hint that the forge API needs a token.
- A rate limit the hub can wait out — up to 120 seconds — is waited out and
  the request tried once more; one it cannot is `429 forge_rate_limited`
  with `Retry-After` in whole seconds where known, and a wait cut short by
  the 30-second deadline is `504 forge_timeout`.
- No response contains the credential or the userinfo of any URL.

### The order of checks

- A read checks: the credential, then a PAT's scope before the workspace is
  looked up, then the workspace, then the target, then the path and query
  parameters, then the forge and its client.
- A write checks: the credential, then a PAT's scope before the workspace,
  then the workspace with a member refused, then that the workspace is not
  archived, then the request, then the forge and its client, then the body
  limit.
- A request refused at any of those steps sends nothing to the forge and
  records no event.

### End-to-end flows

1. **An agent triages an issue.** An agent lists the workspace's open issues
   with a label filter, reads one with its comments, and adds a label.
   Parley resolves the forge and the workspace's credential for each call,
   sends only the parameters given, and records one event for the label
   write.
2. **An agent opens a pull request upstream.** On a `carry_patch` workspace,
   an agent opens a pull request with `?target=upstream`; Parley uses the
   upstream URL, the upstream credential and the upstream API URL setting,
   and answers with the created pull request.
3. **The forge refuses and the caller is told why.** A write is attempted
   with a workspace credential the forge rejects; Parley answers `502
   forge_unauthenticated` with the forge's message scrubbed and the hint
   about storing a token, records the event with that error type, and
   discloses no credential.

## Design Decisions

1. **The forge decision procedure is one function with explicit inputs**,
   because it is used by eighteen routes and its failure is a documented
   error type rather than a fallback.
2. **The two multi-request writes are specified by their ordering**, not
   their outcome: a close with a comment and a label removal each make more
   than one request to the forge, and which one happened when is what the
   caller and the event see.
3. **A write that reached the forge records an event even when it failed.**
   The reference requires it, and it is the only way an operator can see
   that the hub tried.
4. **Nothing is cached**, as the reference states. A rotated secret must
   take effect on the next call, and a cache of the forge client would hide
   that.

## Dependencies

- `01_service_foundation` for the router and the error envelope.
- `02_identity_and_credentials` for the caller's scopes.
- `03_workspaces` for the workspace, its access decision and the two API URL
  settings.
- `05_repository_store` for the origin and upstream credential resolution.
- `12_audit_and_events` for the recorder this package calls on every write
  that reached the forge.
- GitHub's and GitLab's HTTP APIs, at deployment time.
- `docs/prd/api.md` § Forge Endpoints is normative, and `docs/prd/cli.md`
  § Forge Commands for what `afc` expects.

## Verified External API

Nothing was verified against installed code. This package calls GitHub's and
GitLab's REST APIs directly over HTTP rather than through a client library,
so what must be confirmed is not a library's signatures but each forge's own
contract: the paths and payload shapes for issues, comments, labels, pull
requests, files, checks and reviews; the rate-limit headers each returns;
and the body limits of 65,536 code points on GitHub and 1,000,000 on GitLab
that the API reference states. The first task of this package confirms each
against the forge's current documentation and records any divergence.
