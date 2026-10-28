# 12. What the two forges send with a delivery

## Status

Accepted.

## Context

The webhook ingress is the one route Parley serves without a bearer
credential: anyone on the internet can call it, and the forge proves
itself with a secret the workspace holds. Everything the route does
before it trusts a delivery rests on headers the forges send and on
the bytes of the body arriving unmodified, so both were confirmed
against each forge's current documentation before the route was
written.

## What GitHub sends

| Header                                                                       | Holds                                                                                |
| ---------------------------------------------------------------------------- | ------------------------------------------------------------------------------------ |
| `X-GitHub-Delivery`                                                          | a GUID for this delivery                                                             |
| `X-GitHub-Event`                                                             | the event name, such as `issues`                                                     |
| `X-Hub-Signature-256`                                                        | `sha256=` and the hex HMAC-SHA256 of the **request body** under the webhook's secret |
| `X-Hub-Signature`                                                            | the same over SHA-1, kept for compatibility                                          |
| `X-GitHub-Hook-ID`, `…-Installation-Target-Type`, `…-Installation-Target-ID` | the hook and what it was created on                                                  |
| `User-Agent`                                                                 | always begins `GitHub-Hookshot/`                                                     |

The signature is over the body as sent. Parley therefore verifies
over the raw bytes and never over a re-serialised body: two JSON
documents that mean the same thing have different bytes and so
different signatures, and re-serialising would reject every genuine
delivery whose spacing differs from ours.

The SHA-1 header is **not** accepted. The reference says so, and a
service that accepts both offers an attacker the weaker one.

## What GitLab sends

| Header                | Holds                                                                |
| --------------------- | -------------------------------------------------------------------- |
| `X-Gitlab-Event`      | the event name, as `<Event> Hook` — `Push Hook`, `Issue Hook`        |
| `X-Gitlab-Token`      | the secret itself, in plain text, when one is configured             |
| `X-Gitlab-Event-UUID` | a UUID shared by webhooks a webhook triggered                        |
| `Idempotency-Key`     | the delivery's own id                                                |
| `webhook-id`          | the same value as `Idempotency-Key`; GitLab now recommends this name |

GitLab authenticates by sending the secret rather than a signature
over the body, which is why the token is compared before a byte of
the body is read, and why the secret may hold only printable ASCII
from `!` to `~`: it travels in a header.

## The divergences that matter

1. **GitLab's third id header.** The reference names `Idempotency-Key`
   and then `X-Gitlab-Event-UUID`; GitLab's own documentation now
   recommends `webhook-id`, which carries the same value as
   `Idempotency-Key`. Parley reads the reference's two, in its order.
   A deployment that sees GitLab drop `Idempotency-Key` would need
   `webhook-id` added here.
2. **A signature is over bytes, not over JSON.** Confirmed above, and
   the reason the ingress reads the body as an `ArrayBuffer` and
   parses a copy.
3. **GitLab proves nothing about the body.** Its token says who sent
   the delivery and nothing about what it says, so a GitLab delivery
   is as trustworthy as the secret and no more. Nothing in Parley acts
   on a delivery, which is what keeps that acceptable.

## Decision

Verify GitHub over the raw request bytes with HMAC-SHA256 and compare
in constant time; verify GitLab by comparing its token with the
secret in constant time, before the body is read. Read the delivery
id and event name from the headers above, in the reference's order.

## Consequences

A forge that changes a header name changes one table in
`src/webhook/verify.ts`. The ingress reads the body only after the
sender is established, so an unsigned request costs almost nothing to
refuse — which matters for a route that is open to anyone.
