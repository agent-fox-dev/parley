---
spec_id: "53"
spec_name: "event_webhooks"
title: "Deliver events to subscribers by webhook"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "9fb2e1e3c9890aa51dc61ef803d881f615166f3318f12ef2bb4fd610ae94d9d3"
schema_version: 3
source: "docs/prd/14_sessions_audit_events.md"
---

# Deliver events to subscribers by webhook

## Intent

Some consumers of hub events cannot hold a stream open or poll: a CI system, a chat bot, a small script behind a URL. This package lets the owner of a workspace subscribe a URL to the events of that workspace and its task forks. The hub then pushes each event to the URL as a signed HTTP `POST`, in the order the events were written, and keeps trying until the receiver accepts it or a fixed time has passed. A subscription can never be used to reach the hub's own network, and its owner can always see what was sent and with what outcome.

## Goals

- The owner of a workspace, or the admin token, creates, lists, reads, changes, switches off and on, and deletes event webhook subscriptions under `/api/v1/workspaces/:slug/webhooks`; nobody else can, and a workspace-bound token never can.
- The hub generates a signing secret per subscription (`whsec_` plus 64 hexadecimal characters), shows it once at creation and once at each rotation, and stores it encrypted.
- Every matching event of the workspace, and of its task forks unless switched off, is sent as one `POST` whose body is the event record and whose `Parley-Signature` header is an HMAC-SHA256 over the attempt's timestamp and the body.
- Per subscription, events arrive in `seq` order, one at a time, at least once; subscriptions do not wait for each other.
- A failed attempt is retried after 10 s, doubling up to 1 h, for 24 hours per event; then the event is given up and the subscription is `failing`. After 72 hours without a success the subscription is `disabled`.
- A destination on a loopback, link-local, private, unique-local, unspecified, multicast or cloud-metadata address is refused at creation, at a change of the URL and at every connection, unless the operator allows it.
- The owner sends a test delivery (ping), lists the deliveries of the last 7 days with their outcome, and redelivers one.
- A subscription's position and pending delivery survive a restart.
- `parley webhook create|list|get|update|delete|rotate-secret|ping|deliveries|redeliver` drive the routes.

## Non-goals

- The event log, the event record and its JSON form, the hub-wide `seq`, writing an event in the transaction of its state change, the visibility rule, the `types` filter syntax (AUD-FR-042), `GET /events` and `GET /events/log`, and the catalogue entry of the `parley.webhook.*` types: package 52 `durable_event_log_and_stream`. This package reads the log in-process with `ReadAfter`, decides with `VisibleToOwnerOf`, is woken through `OnCommit` and emits its five events with `Emit` in the transaction of their state change, the names package 52 fixes in its Design Decision 27. The `parley.webhook.*` events are owner-only in package 52's visibility rule, with `parley.secret.*` and `parley.token.*`.
- Named secrets, the encryption key, key rotation and the `secret_reencryption` maintenance task: package 39 `secrets`. This package seals the signing secret with its `SealValue`, opens it with its `OpenValue` and registers a converter with its re-encryption task, so that stored signing secrets are re-sealed after a key rotation (Design Decision 21).
- The webhooks the hub receives from a forge (`/hooks/forge/:slug`): package 43 `forge_webhook_ingress`.
- The retention run and the order of its steps (§14.7.2), the Prometheus registry, the metrics catalogue and `GET /metrics` (§14.7.3): package 51 `audit_query_and_retention`. This package registers its delivery store as the `DeliveryPruner` that the `webhook_deliveries` retention step calls, and reports `parley_webhook_deliveries_total` and `parley_webhook_queue_depth` to package 51's recorder and registry.
- Running the removal of a workspace's data when the workspace is deleted (WS-FR-065): package 14 `workspace_archive_reclone_delete`. This package supplies the remover for subscriptions, deliveries and signing secrets and registers it in that package's `Teardown` set from `internal/hub/webhook.go`.
- The access rule and its levels: package 07 `workspace_access_rule`. The binding guard that answers a workspace-bound token: package 08 `workspace_bound_tokens`. The scope registry and scope check: package 06 `api_keys_and_access_tokens`.
- The error envelope, the order of checks, list paging and `Idempotency-Key` (PLAT-FR-010 to PLAT-FR-019): package 02 `http_api_conventions`. CLI settings, output and exit codes (PLAT-FR-090 to PLAT-FR-095): package 09 `cli_foundation`.
- The configuration loader, schema steps, start hooks, background work and shutdown: package 03 `server_lifecycle_and_configuration`. This package registers four keys, its schema steps, one start hook and its background work with them.
- Replay of events written while a subscription was disabled, a switch that turns TLS verification off, following redirects, and storing payloads or response bodies: excluded by the source PRD (AUD-FR-068, AUD-FR-072, §14.3.9).

## Background

The repository holds no code yet; every file named here is new. The event component arrives with package 52 in `internal/event/`, the encryption of secrets with package 39 in `internal/secret/`, the access component with package 07 in `internal/access/` (its workspace record carries `kind`, `parent_slug` and `status`), the router, error envelope, list and idempotency helpers with package 02 in `internal/server/`, the configuration key registry, the store, start hooks and background work with package 03 (`internal/config/`, `internal/store/`, `internal/hub/`), and the CLI root, HTTP client, output and exit codes with package 09 in `internal/cli/`.

This package adds `internal/webhook/` (subscriptions and their routes, destination rules, the sender, the delivery loop, states, ping, the delivery log and redelivery), the command file `internal/cli/webhook.go`, and the end-to-end test `test/e2e/webhook_test.go`.

Source: `docs/prd/14_sessions_audit_events.md`, AUD-FR-062 to AUD-FR-076 and §14.3.9, with the user stories AUD-US-11 and AUD-US-12, the webhook routes of §14.5, the commands of §14.6, the background behaviour of §14.7.1 and §14.7.4, the events of §14.8.10, the `[webhooks]` keys of §14.9 and the event webhook edge cases of §14.10; NFR-044 of `docs/prd/15_non_functional.md` states the same security rules.

## Requirements

### Access and addressing (AUD-FR-062, AUD-FR-063)

All nine routes are under `/api/v1/workspaces/:slug/webhooks`. Each needs level `owner` or `admin` on the workspace: the workspace's owner or the admin token. An org member gets `403 forbidden`; a caller with no access gets the `404 not_found` of the access rule. A workspace-bound token gets `403 token_bound`, whoever its user is. A PAT needs `workspaces:write` for the writing routes (create, update, delete, rotate-secret, ping, redeliver) and `workspaces:read` or `workspaces:write` for the reading routes (list, get, deliveries); without it the answer is `403 insufficient_scope`.

A task fork has no event webhooks: every route called with a fork's slug answers `400 not_supported_on_fork`. A subscription of the parent with `include_forks` covers the fork.

A `:id` or `:delivery_id` that is not a UUID answers `400 invalid_request`. An id that names no subscription of the addressed workspace, also when it names one of another workspace, answers `404` with `error_type` `webhook_not_found`.

### The subscription (§14.3.9)

A subscription has `id` (UUID), `workspace` (slug, never a fork), `url`, `types` (default `["*"]`), `include_forks` (default `true`), `description` (default `""`), `state` (`active`, `failing` or `disabled`; `active` at creation), `created_by` (the caller's user id, null for the admin token), `created_at`, `updated_at`, `last_delivery_at` (null), `last_status` (`succeeded`, `failed` or null) and `failing_since` (null). The signing secret is not a field.

Validation, each violation `400 invalid_request` naming the field:

- `url`: required; an absolute `https://` URL with a host, without userinfo and without a fragment, at most 2,048 characters. `http://` is accepted only when `[webhooks].allow_insecure` is true and the host is a loopback address.
- `types`: a list of at most 20 strings, each an exact type or a prefix ending in `*`, `*` only as the last character.
- `include_forks`: a bool. `description`: a string of at most 1,024 characters.

### Create (AUD-FR-064, AUD-FR-065)

`POST /workspaces/:slug/webhooks` with `url` and optionally `types`, `include_forks`, `description` answers `201` with the subscription plus `secret`. The secret is 32 random bytes shown as `whsec_` followed by 64 lower-case hexadecimal characters. The route accepts `Idempotency-Key`; a replay answers `200` with the subscription without `secret` and creates nothing. The create writes `parley.webhook.create` (metadata `webhook_id`, `host`, `types`, `include_forks`).

Failures: `400 invalid_request` (validation); `400 webhook_destination_refused` (below); `409 workspace_archived` (the workspace is archived); `429` with `error_type` `webhook_limit_reached` when the workspace already has `[webhooks].max_per_workspace` subscriptions in whatever state (default 20; with `0` every create is refused); `503 secrets_unavailable` when no encryption key is configured (SEC-FR-011). A refused create stores nothing and writes no event.

The secret appears in the create response and in the rotate response and nowhere else: in no other response, no event and no log line. It is stored encrypted with the key that encrypts secrets.

### Destination safety (AUD-FR-066, AUD-US-12, NFR-044)

The hub refuses a URL whose host is, or resolves to, a loopback, link-local, private, unique-local, unspecified, multicast or cloud-metadata address, over IPv4 and IPv6. `[webhooks].allow_private = true` lifts the refusal for all of them. `[webhooks].allow_insecure = true` accepts `http://` URLs whose host is a loopback address and lifts the refusal for exactly those.

The check runs when a subscription is created, when its `url` is changed, and at every delivery attempt on the address the hub is about to connect to, so that a name that later resolves elsewhere is caught. At create and update the refusal is `400` with `error_type` `webhook_destination_refused`. At delivery the attempt fails with `error` `destination_refused`, no connection is made, and the attempt is retried like any failure. When a name resolves to several addresses, the hub checks the one it connects to.

### Read, update, delete, rotate (§14.5, AUD-FR-065, AUD-FR-072)

- `GET …/webhooks` lists the subscriptions in (`created_at`, `id`) ascending order as a list envelope, optionally filtered by `state`; a `state` outside the three values is `400 invalid_request`. No item carries a secret.
- `GET …/webhooks/:id` answers the subscription plus `pending`, the number of events waiting to be sent to it.
- `PATCH …/webhooks/:id` takes at least one of `url`, `types`, `include_forks`, `description`, `active`. Fields not given keep their value. A changed `url` or filter applies from the next attempt; the pending delivery is kept and is still sent. A changed `types` applies to every event not yet examined. It answers `200` with the subscription and writes `parley.webhook.update` (metadata `webhook_id`, `fields`: the names of the fields given, `state`). Failures: `400 invalid_request` (a bad value, an unknown field, no field); `400 webhook_destination_refused`; `409 workspace_archived`.
- `PATCH` with `active: true` sets a `disabled` or `failing` subscription to `active` and clears `failing_since`. A subscription that was `disabled` resumes with the first event written after the re-enable; events written while it was disabled are not sent. `PATCH` with `active: false` sets `disabled` and drops the pending delivery.
- `DELETE …/webhooks/:id` answers `204` in any workspace status, removes the subscription with its pending delivery, its delivery log and its signing secret, and writes `parley.webhook.delete` (metadata `webhook_id`, `host`).
- `POST …/webhooks/:id/rotate-secret` (no body) answers `200` with the subscription plus the new `secret`; the old secret stops being used with the next attempt, also for a delivery that is being retried. It writes `parley.webhook.update` with `fields: ["secret"]`. Failures: `409 workspace_archived`; `503 secrets_unavailable`.

Event metadata holds the host of the URL, never the URL, and never the secret (§14.8.10). The `parley.webhook.*` events carry `resource_type` `event_webhook`, `resource_id` the subscription id and `workspace` the subscription's workspace.

### The delivery request (AUD-FR-067, AUD-FR-068)

A delivery attempt is one HTTP `POST` to `url`. The body is the hub event record as one JSON object, exactly as the event log returns it, with `Content-Type: application/json`. The headers are:

| Header | Value |
|---|---|
| `Parley-Event` | the event's `type` |
| `Parley-Delivery` | the delivery id; the same on every retry and on a redelivery |
| `Parley-Seq` | the event's `seq` |
| `Parley-Timestamp` | Unix time in seconds at which this attempt was sent |
| `Parley-Signature` | `sha256=` followed by the lower-case hexadecimal HMAC-SHA256 of `<timestamp>.<body>` with the signing secret |
| `User-Agent` | `parley-webhook/<hub version>` |

The hub sends no credential of its own and no other header that identifies a user. Each attempt carries a new timestamp and a signature over it.

An attempt succeeds when a response with a `2xx` status arrives within `[webhooks].timeout` (default `10s`, `1s` to `60s`, measured from connect to the response status). Every other outcome is a failure: another status, a timeout (`error` `timeout`), a connection error (`connection_failed`), a TLS error (`tls_error`), a refused destination (`destination_refused`). Redirects are not followed: a `3xx` answer is a failure with `error` `redirect`. The response body is not read beyond what is needed to finish the request and is never stored. TLS certificates are verified; there is no switch to turn that off.

### What a subscription receives, and in what order (AUD-FR-069, §14.3.9, §14.7.4)

A subscription receives exactly the events that an owner of its workspace could see for it and that match `types`: every event whose `workspace` is the subscription's workspace, including the owner-only types (`parley.secret.*`, `parley.token.*`, `parley.webhook.*`); with `include_forks`, every event whose `workspace` is a task fork of that workspace, also after the fork is gone; never an event without a workspace and never an event of another workspace. It starts with the first event written after it was created and never sends older events. Its own `parley.webhook.*` events are sent to it like any other.

Each subscription has a position in the event log. A delivery loop runs for every `active` and `failing` subscription, driven by new events and by its own retry timer:

1. It reads the next event after the position that the subscription may receive and that matches `types`. Events that do not match move the position forward without a delivery.
2. It records a `pending` delivery and makes the first attempt.
3. On success it marks the delivery `succeeded`, moves the position past the event, sets `last_status` and `last_delivery_at`, clears `failing_since` and returns a `failing` subscription to `active`.
4. On failure it sets `last_status` and, if it is null, `failing_since`, and schedules the next attempt.

The next event is sent only when the current one has succeeded or been given up. A receiver may get an event more than once and never gets event B before event A when A has the lower `seq`, except through a redelivery. At most one attempt per subscription is in flight; a slow or dead receiver delays only its own subscription. An event that has left the event log before its turn came is skipped and counted as `failed`.

A delivery record has `id` (UUID, sent as `Parley-Delivery`), `seq` (`0` for a ping), `type`, `status` (`pending`, `succeeded`, `failed`), `attempts`, `response_code` (null when no response arrived), `error` (`timeout`, `connection_failed`, `tls_error`, `destination_refused`, `redirect` or null), `duration_ms`, `created_at`, `last_attempt_at` and `next_attempt_at` (null unless `pending`). It holds no payload and no response body.

### Retries, failing and disabled (AUD-FR-070, AUD-FR-071)

After a failed attempt the hub waits 10 s, then twice as long after each further failure, up to 1 h between attempts. It retries one event for up to 24 hours, counted from its first attempt. Then it gives the event up: the delivery becomes `failed`, the subscription becomes `failing` if it is not already, `parley.webhook.failing` is written on that transition (actor `system`; metadata `webhook_id`, `host`, `seq`, `failing_since`, `response_code`, `error`), and the hub goes on with the next event, which again gets up to 24 hours.

When no attempt of a subscription has succeeded for 72 hours since `failing_since`, the hub sets it `disabled`, marks its `pending` delivery `failed` and writes `parley.webhook.disabled` (actor `system`; metadata `webhook_id`, `host`, `failing_since`). A timer checks at least once a minute. Any successful attempt before that clears `failing_since` and returns a `failing` subscription to `active`. A `disabled` subscription gets no deliveries, and events written meanwhile are not kept for it.

### Durability and the workspace's life (AUD-FR-075, §14.7.1, §14.10)

A subscription's position and its pending delivery survive a restart. At start the hub resumes delivery for every `active` and `failing` subscription from its stored position; an attempt that was in flight when the hub stopped is made again. At shutdown the hub abandons attempts in flight without waiting for them.

When the workspace is deleted, its subscriptions go in the same step, with whatever they had not delivered, and emit no event of their own. While the workspace is archived, subscriptions stay and keep delivering; they cannot be created or changed, and can be read, pinged and deleted.

Configuration, read at start: `[webhooks].max_per_workspace` (default `20`, ≥ 0), `[webhooks].timeout` (default `10s`, `1s` to `60s`), `[webhooks].allow_private` (default `false`), `[webhooks].allow_insecure` (default `false`). A value outside these ranges stops the start.

### Metrics (AUD-FR-076)

Every attempt is counted in `parley_webhook_deliveries_total{result}`: `ok` for a successful attempt, `retry` for a failed attempt that will be retried, `failed` for an attempt after which the event was given up, a failed ping or redelivery, and an event skipped because it left the log. `parley_webhook_queue_depth` is the number of events written and not yet delivered or given up, summed over all `active` and `failing` subscriptions.

### Ping, the delivery log and redelivery (AUD-FR-073, AUD-FR-074)

`POST …/webhooks/:id/ping` (no body) sends one test delivery at once, outside the order of the subscription, in any subscription state and any workspace status. Its body has the shape of an event record with `type` `parley.webhook.ping`, `seq` `0`, a new `id`, the workspace, the caller as actor, `resource_type` `event_webhook`, `resource_id` the subscription id and empty metadata. It is signed like any delivery, is not written to the event log, is not retried, and does not change `state`, `last_status` or `failing_since`. The route answers `200` with the delivery record once the attempt has ended, with `status` `succeeded` or `failed`: a failed ping is a `200`.

`GET …/webhooks/:id/deliveries` lists the delivery records, pings included, newest first ((`created_at`, `id`) descending), optionally filtered by `status`; a `status` outside the three values is `400 invalid_request`. Records are kept for 7 days after their last attempt; `pending` records are never deleted.

`POST …/webhooks/:id/deliveries/:delivery_id/redeliver` (no body) sends the event of a `succeeded` or `failed` delivery once more, at once and outside the order, with the same delivery id, in any subscription state, and answers `200` with the updated record. It does not change the subscription's position or state. Failures: `404` with `error_type` `delivery_not_found`; `409` with `error_type` `delivery_pending` for a `pending` delivery; `410` with `error_type` `event_expired` when the event has left the event log; `400 invalid_request` for a ping.

### CLI (§14.6)

| Command | Request | Flags and output |
|---|---|---|
| `parley webhook create <workspace-slug>` | `POST …/webhooks` | `--url <url>` (required), `--types <list>` (comma-separated), `--no-forks` (sends `include_forks: false`), `--description <text>`. Prints the response, which includes `secret`; a note on stderr says the secret is shown only once |
| `parley webhook list <workspace-slug>` | `GET …/webhooks` | `--state`, `--limit`, `--cursor` |
| `parley webhook get <workspace-slug> <webhook-id>` | `GET …/webhooks/:id` | prints the subscription |
| `parley webhook update <workspace-slug> <webhook-id>` | `PATCH …/webhooks/:id` | `--url`, `--types`, `--forks` and `--no-forks` (mutually exclusive), `--description`, `--enable` and `--disable` (mutually exclusive; `active: true` and `active: false`). At least one flag, else exit 2 |
| `parley webhook delete <workspace-slug> <webhook-id>` | `DELETE …/webhooks/:id` | prints `{"status":"deleted","id":"<webhook-id>"}` |
| `parley webhook rotate-secret <workspace-slug> <webhook-id>` | `POST …/rotate-secret` | prints the response with the new `secret`; the same note on stderr |
| `parley webhook ping <workspace-slug> <webhook-id>` | `POST …/ping` | prints the delivery record; exit 0 when it `succeeded`, exit 1 when it `failed` |
| `parley webhook deliveries <workspace-slug> <webhook-id>` | `GET …/deliveries` | `--status`, `--limit`, `--cursor` |
| `parley webhook redeliver <workspace-slug> <webhook-id> <delivery-id>` | `POST …/redeliver` | prints the delivery record; exit codes as `ping` |

### End-to-end flows

1. **Subscribe, receive, inspect.** A developer runs `parley webhook create demo --url <receiver>`; the CLI posts to the hub; `parleyd` validates the destination, stores the subscription with its encrypted secret and answers `201` with the secret. The developer changes the subscription's description; `parleyd` writes `parley.webhook.update` and its delivery loop posts that event to the webhook receiver, signed; the receiver verifies the signature with the secret and answers `200`. The developer runs `parley webhook ping` and `parley webhook deliveries` and sees the event's delivery and the ping, both `succeeded`.
2. **A receiver that is down, and comes back.** A webhook receiver answers `500`. `parleyd` retries the first event with growing waits, gives it up after 24 hours and marks the subscription `failing`; after 72 hours without a success it disables the subscription. The developer runs `parley webhook update --enable`; the receiver answers `200` again; the next event written is delivered, the events given up are not, and the developer redelivers one of them with `parley webhook redeliver`.

## Design Decisions

1. **Language and stack.** Go; the transactional store is SQLite through `modernc.org/sqlite` behind `internal/store`; HTTP is `net/http`; the CLI is `github.com/spf13/cobra`; tests use `testing` with tables, `net/http/httptest` for the routes and for webhook receivers, and a replaceable clock. Why: project-wide decisions.
2. **Order of checks on the routes.** Binding guard (`403 token_bound`), scope, access with need `owner` (`404`, `403 forbidden`), then fork (`400 not_supported_on_fork`), then the shape of `:id` and `:delivery_id` (`400`), then the lookup (`404 webhook_not_found`, `404 delivery_not_found`), then the body (`400 invalid_request`, then `400 webhook_destination_refused`), then state (`409 workspace_archived`, `429 webhook_limit_reached`, `503 secrets_unavailable`, `409 delivery_pending`, `410 event_expired`). Why: the source PRD lists the errors without an order; this follows PLAT-FR-013, and a fork can never have a subscription whatever the body says.
3. **Where a new subscription starts.** Its position is the `seq` of its own `parley.webhook.create` event, so the first event it can receive is the next one written. A re-enabled `disabled` subscription takes the `seq` of the `parley.webhook.update` event of the re-enable as its position. Why: "the first event written after" needs one exact point, and the event of the request itself is that point.
4. **The HMAC key** is the secret as shown: the ASCII bytes of `whsec_` and the 64 hexadecimal characters. Why: that string is what the receiver holds; the PRD does not say whether the key is the string or the 32 bytes.
5. **The end of the 24 hours.** The next attempt is due at the earlier of "last attempt plus the wait" and "first attempt plus 24 hours". A failed attempt made 24 hours or more after the first one gives the event up. Why: the PRD says "up to 24 hours"; this gives every event one last attempt at the 24-hour mark and makes the give-up time exact.
6. **`active: false` marks the pending delivery `failed`** with `next_attempt_at` null, as the 72-hour disabling does. Why: "drops" has no fourth delivery status; the record stays in the delivery log. `failing_since` is left as it is until a re-enable clears it. `active: true` on an `active` subscription and `active: false` on a `disabled` one change nothing and still answer `200` and write the update event.
7. **A `3xx` answer** is recorded with `response_code` the status and `error` `redirect`. Any other non-`2xx` status is recorded with `response_code` and `error` null.
8. **Ping and redelivery change nothing on the subscription**: not `state`, `last_status`, `failing_since`, `last_delivery_at` or the position. Why: the PRD says so for ping except for `last_delivery_at`, which is paired with `last_status`; redelivery is "outside the order" in the same way. A redelivery sets the record's `status` to the outcome of its attempt, adds one to `attempts` and sets `last_attempt_at`, so the 7 days count again.
9. **Destination check at create and update.** A URL that fails the shape rules is `invalid_request` before the destination is looked at. The host is resolved; the URL is refused when any resolved address is refused. A name that does not resolve is accepted: its attempts fail with `connection_failed`. Why: the PRD names only "resolves to" a refused address, and a resolver outage must not block configuration.
10. **Refused ranges.** IPv4 `127.0.0.0/8`, `169.254.0.0/16` (which holds the metadata address `169.254.169.254`), `10.0.0.0/8`, `172.16.0.0/12`, `192.168.0.0/16`, `0.0.0.0`, `224.0.0.0/4`; IPv6 `::1`, `fe80::/10`, `fc00::/7` (which holds `fd00:ec2::254`), `::`, `ff00::/8`; an IPv4-mapped IPv6 address is judged by its IPv4 address. For `allow_insecure`, "a loopback address" is a literal address in `127.0.0.0/8`, `::1`, or the name `localhost`.
11. **An event that left the log.** When the stored position is older than the oldest event of the log, the position moves to just before the oldest event and one `failed` is counted; a pending delivery whose event has left the log is marked `failed`, counted, and passed. Neither makes the subscription `failing`. Why: the hub cannot enumerate events it can no longer read.
12. **A signing secret that cannot be decrypted** (no key, or a key that matches neither configured key): ping and redeliver answer `503 secrets_unavailable`; the delivery loop makes no attempt, leaves the delivery as it is and tries again within a minute. Why: SEC-FR-013 fails any operation that needs the value; an unsigned delivery must never leave the hub, and the `error` values of a delivery have none for this.
13. **`pending` of `GET …/webhooks/:id`** counts the events after the position that the subscription may receive and that match `types`, the one with a pending delivery included; it is `0` for a `disabled` subscription.
14. **Metrics and retention: this package registers itself with package 51.** Package 51 must not depend on this package, so this package depends on package 51 (added to the plan's list) and connects itself in `internal/hub`. Attempts are reported with the recorder of `internal/metrics`: `Add` on `parley_webhook_deliveries_total` with the label `result`. The queue depth is the function `QueueDepth`, registered with `RegisterSource` for `parley_webhook_queue_depth`. The delivery store satisfies `audit.DeliveryPruner`: `PruneDeliveries(ctx, before)` deletes the records that are not `pending` and whose last attempt is before the given time and returns the number deleted; it is registered with `audit.RegisterDeliveryPruner`, and the `webhook_deliveries` step passes now minus 7 days. A successful ping or redelivery counts as `ok`.
15. **Workspace deletion through the `Teardown` set of package 14.** The component offers a remover in the shape package 14 declares in `internal/workspace/teardown.go` for a `Remover` of owned data: it takes the removal transaction and one slug and returns an error only. `internal/hub/webhook.go` registers it in the `Teardown` set, always: package 14 is built before this one and names the event webhooks among the owners that register their remover themselves, so the registration is unconditional. Package 14 calls the remover inside the removal transaction for a deleted workspace and, through its `RemoveOwned`, for each fork that goes away. The plan gives this package no dependency on package 14; the registration is made in the hub's composition, which may import both.
16. **The sender's trust roots, resolver and dialer are constructor parameters**, not configuration. Production uses the system roots. Why: tests must reach a TLS test server without a switch that turns verification off.
17. **`parley webhook ping` and `redeliver` on a failed delivery** print the record on stdout, report an envelope with `code` 0 and `error_type` `delivery_failed` on stderr and exit 1. Why: the PRD fixes exit 1; package 09's error types have none for it.
18. **An empty `types` list** and an empty entry are `400 invalid_request`. On create, unknown body members are ignored (PLAT-FR-017); on `PATCH` an unknown field is `400`, as §14.5 states.
19. **How the loop learns of new events.** It is woken by the event component's in-process notification of a committed event (`OnCommit` of package 52), the one its stream uses; no polling interval is part of the contract.
20. **Profile: full.** The package has ten requirements and two end-to-end flows.
21. **Signing secrets follow a key rotation.** Package 39's re-encryption task accepts registered converters (its Design Decision 16) so that "zero remaining" covers every value sealed under the previous key. This package registers one: it finds them with `NeedsReseal` and re-seals under the current key every signing secret still sealed under the previous key and returns the numbers converted and remaining. A ciphertext that opens under neither key is left as it is and counted as remaining. The conversion changes no member of a subscription and writes no event.
22. **Signing secrets use package 39's names.** A signing secret is not a named secret: it is stored in the subscription's row, sealed with `secret.SealValue` and opened with `secret.OpenValue` under the binding string `webhook:<subscription id>`, so a ciphertext copied to another subscription does not open. `secret.Available` decides the `503 secrets_unavailable` of create and rotate; `ErrUnavailable` and an error matching `ErrUnreadable` from `OpenValue` are the "cannot be decrypted" of decision 12; `NeedsReseal` tells the converter which secrets are still under the previous key. `Get` and `Put` are that package's operations for named secrets, under those names wherever hub code reads or stores one; this package stores none and adds no cipher of its own.

## Dependencies

- Package 52 `durable_event_log_and_stream` (draft): the in-process read of events after a sequence number, the visibility rule, the `types` filter, the event record's JSON form, the notification of new events, and emitting the five `parley.webhook.*` events in the transaction of their state change. Through it: the store, configuration, start hooks and background work of package 03 and the access component of package 07.
- Package 39 `secrets` (draft): `SealValue`, `OpenValue` and `NeedsReseal` for the signing secret, `Available` and `ErrUnavailable` for the `secrets_unavailable` condition, and the converter registry of its re-encryption task.
- Package 51 `audit_query_and_retention` (draft): the recorder and registry of `internal/metrics` and the `DeliveryPruner` registration of `internal/audit`. Added to the plan's list so that this package registers itself and package 51 needs no dependency on it.
- Package 09 `cli_foundation` (draft): the HTTP client, output, exit codes and command declaration the nine commands are built on.
- Connected at start, not needed to build or test this package: the binding guard of package 08, the `Teardown` set of package 14, with which `internal/hub/webhook.go` registers the workspace remover.
- External: the receivers' HTTP endpoints, reached with the standard library's HTTP client only.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Event webhook**: A workspace's subscription that makes the hub send each matching hub event to a URL as a signed HTTP `POST`, in order and with retries (§14). Not to be confused with the forge's webhooks, which the hub receives (§12).
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Signing secret**: The per-subscription value the hub generates and signs every delivery with; shown once, stored encrypted.
- **Position**: The `seq` up to which a subscription has examined the event log.
- **Delivery**: The record of one event sent to one subscription, across all its attempts.
