---
spec_id: "14"
spec_name: "forge_webhooks"
title: "Receive a forge's webhook deliveries and relay them as events"
status: "active"
created_at: "2026-10-27T19:08:11Z"
updated_at: "2026-10-28T14:23:51Z"
intent_hash: "4cbd3ab39cb29349d97197c5b37bae3d6fff11b4deb3f0457d8fef6a1cb77fd8"
schema_version: 2
source: "docs/prd/01-port-af-hub-to-typescript-on-cloudflare.md"
---

# Receive a forge's webhook deliveries and relay them as events

## Intent

Polling a forge to notice that an issue was labelled or a pull request
merged is slow, expensive and rate-limited. A webhook inverts it: the forge
tells Parley, and Parley relays it as an audit event that anything watching
the stream already receives. The whole value rests on the ingress being
trustworthy — it is the one route served without a bearer credential, open
to the internet, so its signature check, its duplicate handling and its
refusal to act on what it is told are the feature.

## Goals

- `POST /hooks/forge/:slug` accepts a signed delivery from GitHub or GitLab,
  records it once, answers the forge, and then relays the documented hub
  events, exactly as `docs/prd/api.md` § The webhook ingress specifies.
- The webhook secret is set and removed through the API, and the delivery
  log is readable, as § The webhook routes specifies.
- A delivery is processed at most once, even when several copies arrive
  together.
- The hub never acts on a delivery and never contacts the forge for one.
- `afc forge webhook set`, `unset` and `afc forge deliveries` behave as
  `docs/prd/cli.md` specifies.

## Non-goals

- **Registering the webhook on the forge.** A person enters the URL and the
  secret in the forge's settings; the hub gives them both and calls nothing.
- **Reacting to a delivery.** Nothing is rebuilt, synced or merged because a
  forge said something happened. Relaying is the whole behaviour.
- **The forge's API.** Reading and writing issues and pull requests is
  `13_forge_api`.
- **Forgejo and Gitea, GitLab's signing-token header, GitHub's legacy SHA-1
  signature, form-encoded payloads, an organization-or-user-level secret,
  and rate limiting of the ingress** — all named as unsupported by the
  reference.

## Background

`13_forge_api` built the forge identification and the delivery log's reader
side is new here. `12_audit_and_events` built the recorder and the stream
this package relays into. `04_secrets_and_variables` stores the secret,
which is the ordinary workspace secret `FORGE_WEBHOOK_SECRET`, so a person
may set it with either route.

The ingress is unlike every other route in the service. It is served at the
root, **without bearer authentication** — the forge proves itself with the
workspace's secret — and the API's body-size limit and content-type check do
not apply, so it has a limit of its own. Its checks are ordered so that
nothing reads the body before the sender is established: the workspace and
its secret first, then which scheme by header, then GitLab's token, then the
body up to 1 MiB, and only then GitHub's signature over the raw bytes.

Two behaviours protect the consumer. A delivery is recorded **per workspace
and delivery id** before anything is relayed, so a redelivery or several
simultaneous copies produce one set of events. And the forge is answered
**before** the events are relayed, so a slow audit store cannot hold the
forge's connection open — which means the relay must still happen when the
forge has already gone away.

## Requirements

### Setting and removing the secret

- `PUT .../forge/webhook` stores the workspace's webhook secret, generating
  one of 64 hex characters when the caller supplies none, and answers with
  whether it is configured, where the forge should post, and the generated
  secret if it generated one. A generated secret appears in that answer and
  nowhere else.
- A supplied secret is 16 to 255 printable ASCII characters from `!` to
  `~`. A body that is not a JSON object with an optional string secret, a
  secret of the wrong length or with a disallowed character, a workspace
  whose secrets are full, and an archived workspace are each `400`, with a
  message naming the rule and never the value.
- `DELETE .../forge/webhook` removes the secret and answers `204` whether or
  not there was one, and works on an archived workspace. Recorded
  deliveries are kept.
- The secret is the workspace secret `FORGE_WEBHOOK_SECRET`, so setting it
  through the secrets routes has the same effect, and the hub reads it for
  every delivery so a rotation applies from the next one.

### Reading the delivery log

- `GET .../forge/deliveries` lists the workspace's recorded deliveries
  newest first, with those recorded in the same second ordered by when they
  were recorded, taking a time lower bound and a limit of 1 to 100
  defaulting to 30.
- Each row says the forge's delivery id, the forge, the event name, the
  action, which of the workspace's repositories it was about, what the hub
  made of it, and which events it relayed. The payload is not kept.
- A bad, repeated or unparseable parameter is `400` naming it.
- Deliveries older than 7 days, and those of workspaces that no longer
  exist, are removed every hour.

### Accepting a delivery

- The ingress checks, in order: the workspace exists and has a non-empty
  secret; which scheme the headers say; GitLab's token against the secret in
  constant time; the body up to 1 MiB; and GitHub's signature as the
  HMAC-SHA256 of the raw body under the secret, compared in constant time.
- An unknown slug and a workspace without a secret answer the same `404`
  before the body is read; a store failure is `500`. The workspace's status
  does not matter.
- Neither scheme's header is `401` before the body is read; a missing,
  malformed or wrong signature or token is `401`, whatever is wrong, and
  records and emits nothing.
- A body over 1 MiB is `413 body_too_large`.
- The delivery's id and event name are read from each forge's headers; a
  delivery with neither, or with an id over 255 characters or outside
  printable ASCII, is `400` and records nothing.

### Processing it once

- A delivery is recorded per workspace and delivery id before anything is
  relayed. One whose id is already recorded is answered `202` as a
  duplicate, emitting nothing and adding nothing, and of several copies
  arriving together only one is processed.
- If recording fails the answer is `500` and nothing is emitted, so the
  forge's redelivery is processed in full.

### Answering and relaying

- An accepted delivery is answered `202` with its outcome **before** any
  event is relayed, and the relay happens even if the forge has gone away.
- The outcome is relayed, ignored, repository mismatch, duplicate, or
  malformed for a body that is not one JSON object or that lacks what the
  delivery is about — which is answered `400` and recorded as malformed.
  Descriptive fields that are missing default to empty instead.
- The repository in the payload is compared, ignoring case, with the
  origin's owner and name and, on a `carry_patch` workspace with an
  upstream, with the upstream's.
- The GitHub and GitLab event mappings produce exactly the documented hub
  events, in the documented order, with GitLab's label changes capped at 100
  events per delivery and 100 labels per event. Everything else is ignored.
- The payload itself is never stored or logged, and the hub never contacts
  the forge for a delivery.

### End-to-end flows

1. **A webhook is set up and a delivery arrives.** An operator calls the
   webhook route with no secret; Parley generates one, stores it as the
   workspace secret and returns it once with the URL. The operator enters
   both in the forge. The forge posts a signed `issues`/`labeled` delivery;
   Parley verifies the signature, records the delivery, answers `202
   relayed`, and then relays the labelled event, which reaches a client
   watching the event stream and appears in the audit query and the
   delivery log.
2. **A redelivery changes nothing.** The forge redelivers the same delivery
   id; Parley answers `202 duplicate`, emits no event and adds no row, and
   the delivery log still shows one entry.
3. **An unsigned delivery is refused.** A request arrives for a workspace
   with a secret but with no signature header, and another with a wrong
   signature; both are answered `401`, nothing is recorded and nothing is
   relayed.

## Design Decisions

1. **The checks are ordered so the body is read last.** Everything that can
   be decided from the slug, the stored secret and the headers is decided
   before a byte of the body is read, which is what keeps an unauthenticated
   public route cheap to refuse.
2. **Recording precedes relaying, and the forge is answered in between.**
   The reference requires both. Recording first is what makes the delivery
   exactly-once; answering before relaying is what keeps a slow audit store
   from holding the forge's connection; and the relay must therefore survive
   the forge's disconnection.
3. **A malformed payload is recorded, not discarded.** It is answered `400`
   but kept in the log, because a webhook that silently drops what it cannot
   parse is one nobody can debug.
4. **The hub never acts on a delivery.** It is the simplest defence for a
   route anyone on the internet can call: the worst a forged delivery that
   somehow passed the signature check could do is add a log row and an
   event.

## Dependencies

- `01_service_foundation` for the router and the root-mounted route.
- `03_workspaces` for the workspace and its access decision.
- `04_secrets_and_variables` for the `FORGE_WEBHOOK_SECRET` the ingress
  reads and the webhook route writes.
- `12_audit_and_events` for the recorder and the stream the relayed events
  reach.
- `13_forge_api` for the repository identification the mismatch check
  reuses, and for the description route whose webhook block reads this
  package's log.
- `docs/prd/api.md` § The webhook routes and § The webhook ingress are
  normative, and `docs/prd/cli.md` § afc forge webhook and § afc forge
  deliveries for what `afc` expects.

## Verified External API

Nothing was verified against installed code. This package needs
constant-time comparison and HMAC-SHA256 over raw request bytes; both are
expected from the platform's own cryptography rather than a library, and the
first task confirms that the raw body is available unmodified before the
signature is computed — a signature taken over a re-serialised body silently
fails for every delivery. The header names and payload shapes of GitHub's
and GitLab's webhooks are confirmed against each forge's current
documentation in the same task.
