---
spec_id: "43"
spec_name: "forge_webhook_ingress"
title: "Receive forge webhooks as hub events"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "767a563a5cae76176301b4165c7e397f8eff725bbe827115e6403cfb18590560"
schema_version: 3
source: "docs/prd/12_forge.md"
---

# Receive forge webhooks as hub events

## Intent

A workspace's repository lives on a forge, and what happens there (an issue opened, a label added, a pull request merged, a push) matters to the agents and people working through the hub. The hub receives the forge's webhook deliveries for a workspace, proves each one against a secret only that workspace holds, records it exactly once and relays it as hub events, so that consumers follow forge activity on the same event stream as everything else, without a forge credential and without trusting an unauthenticated caller.

## Goals

- A caller with `forge:write` and write access sets or rotates a workspace's webhook secret with `PUT /api/v1/workspaces/:slug/forge/webhook` and removes it with `DELETE`. A secret the hub generates is 64 lower-case hex characters and is shown in that one response only.
- `POST /hooks/forge/:slug` accepts a GitHub delivery by its HMAC-SHA256 signature and a GitLab delivery by its token, with no bearer credential, and answers `202` with the outcome `relayed`, `ignored`, `repository_mismatch` or `duplicate`.
- Every refusal of the ingress is one of `404 not_found`, `401 unauthenticated`, `413 payload_too_large` and `400 invalid_request`, decided in a fixed order; every `401` has the same message and records nothing.
- A delivery id is recorded at most once per workspace, also under concurrent copies; a repeat is answered `duplicate` and causes no event.
- A delivery recorded as `relayed` always has its `parley.forge.*` events: the record and the events are written in one transaction before the `202`.
- The mapping from GitHub and GitLab events to hub events is the table of FRG-FR-040, and each event carries the metadata of §12.8 and no title of a write, body, comment text or secret.
- `GET /api/v1/workspaces/:slug/forge/deliveries` lists the recorded deliveries newest first; deliveries older than 7 days are pruned by the maintenance loop; deleting a workspace deletes its deliveries.
- `parley forge webhook set`, `parley forge webhook unset` and `parley forge deliveries` call these routes.

## Non-goals

- Forge detection, the repository derived from a remote URL, the forge client, the read routes, the diagnostic route `GET /workspaces/:slug/forge` itself, and the commands `parley forge status|repository`, `parley issue …` and `parley pr …` for reads: package 41 forge_reads. That route computes `webhook.configured` and `webhook.url` itself; this package only registers the provider of `webhook.last_delivery_at`.
- Forge write routes, their events (`parley.forge.issue.create` and the others of the write table) and their commands: package 42 forge_writes.
- Event webhooks, the signed `POST`s the hub **sends** to subscribers (`/workspaces/:slug/webhooks…`, `parley webhook …`): package 53 event_webhooks.
- The event record, `seq`, visibility, the stream and the log routes: package 52 durable_event_log_and_stream. This package appends events through it.
- Secret storage, encryption, the secrets routes and the per-scope limit: package 39 secrets. This package stores and reads one workspace secret through it.
- The maintenance loop and its order of tasks: package 03 server_lifecycle_and_configuration. The workspace delete transaction: package 14 workspace_archive_reclone_delete. The error envelope, check order, list paging and rate limiter: package 02 http_api_conventions. The CLI's client, output and exit codes: package 09 cli_foundation.
- Registering, changing or deleting a webhook on the forge. The hub never does this (FRG-FR-034); a person enters the URL and the secret in the forge's settings.
- Acting on a delivery. A delivery opens, updates, approves or lands no change and triggers no sync (§12.1).
- Forges other than GitHub and GitLab; GitHub's legacy SHA-1 signature; GitHub's form-encoded payloads.

## Background

The repository holds no code yet; every file named here is new, except `internal/cli/forge.go` and `internal/cli/forge_test.go`, which package 41 creates with the `forge` command group and this package extends. The work lives in `internal/forge/` (webhook secret management, the ingress, reduction and mapping, the delivery record and its list route), `internal/cli/forge.go` (the three commands, added to the `forge` command group of package 41) and `test/e2e/` (the end-to-end test).

The source is `docs/prd/12_forge.md`, requirements FRG-FR-032 to FRG-FR-041, with the routes of §12.5, the commands of §12.6, the background behaviour of §12.7, the webhook and delivery events of §12.8, `server.external_url` of §12.9 and the ingress cases of §12.10. The conventions of `docs/prd/03_platform.md` (PLAT-FR-010 to PLAT-FR-019, PLAT-FR-090 to PLAT-FR-095) hold and are not restated.

This package plugs into components other packages build: the route registration, error helper, list helper and rate limiter of package 02 (`internal/server`); the secret component of package 39 (`internal/secret`), used under its own names: `Get` to read the secret, `Put` to store it and `Delete` to remove it inside this package's transaction, `Available` for the key check; the event component of package 52 (`internal/event`); the maintenance task registry of package 03 (`internal/maintenance`), where the name `forge_delivery_pruning` already has its place in the order; the registry of removers of owned data of package 14 (`internal/workspace`); the repository derivation of package 41 (`internal/forge`).

## Requirements

### Webhook secret

The webhook secret of a workspace is the workspace-scope secret `FORGE_WEBHOOK_SECRET` (§12.3, §11). The **webhook URL** is `server.external_url` without a trailing `/`, followed by `/hooks/forge/<slug>`; it is `null` when `external_url` is empty. A secret can be set either way.

**Set or rotate (FRG-FR-032).** `PUT /api/v1/workspaces/:slug/forge/webhook` needs the scope `forge:write` and write access to the workspace. With no body, with `{}` or with `{"secret": null}` the hub generates a secret: 32 random bytes written as 64 lower-case hex characters. A given `secret` must be a JSON string of 16 to 255 characters, each in the range `!` to `~` (printable ASCII, no space). The hub stores the value as the workspace secret, replacing an earlier value, and answers `200` with `{"configured": true, "url": string|null, "secret"?: string}`. `secret` is present only when the hub generated it, and no later response of any route shows it. The route records `parley.forge.webhook.set` (actor the caller, `resource_type` `webhook`, `resource_id` the slug, `action` `set`, metadata `generated`). No `parley.secret.*` event is recorded (§11.8). Neither the event nor any log line holds the value.

Failures, in the order they are decided after authentication:

| Condition | Answer |
|---|---|
| a PAT without `forge:write` | `403 insufficient_scope` |
| no read access to the workspace | `404 not_found` |
| read access only | `403 forbidden` |
| the slug names a task fork | `400 fork_has_no_origin` |
| the body is not a JSON object, or `secret` is neither absent, `null` nor a string that meets the rule | `400 invalid_request`; the message never contains the value |
| the workspace is archived | `409 workspace_archived` |
| no encryption key is configured | `503 secrets_unavailable` |
| the workspace has 100 secrets and none is `FORGE_WEBHOOK_SECRET` | `429 secret_limit_reached` |

A refused request stores nothing and records no event.

**Remove (FRG-FR-033).** `DELETE /api/v1/workspaces/:slug/forge/webhook` needs `forge:write` and write access and works in any workspace state, archived included. It removes the secret and answers `204` whether or not one existed. It records `parley.forge.webhook.unset` (`action` `unset`, metadata `existed`) in both cases. Recorded deliveries remain. On a task fork it answers `400 fork_has_no_origin`. From then on the ingress answers `404` for the workspace.

Neither route, nor the ingress, sends any request to the forge (FRG-FR-034, FRG-FR-035).

### Ingress

`POST /hooks/forge/:slug` is public and lies outside `/api/v1` (FRG-FR-035). It takes no bearer credential and ignores one that is sent. It is exempt from the request rate limit (PLAT-FR-025). It does not check the content type. Its errors use the error envelope. The secret is read on every delivery, so a rotation applies to the next delivery. An archived workspace still relays.

The ingress decides in this order (FRG-FR-036), and the first match answers:

1. Unknown slug, a task fork's slug, or a workspace whose `FORGE_WEBHOOK_SECRET` is absent or empty: `404 not_found`, with one identical body for all three.
2. Scheme. A request with the header `X-Hub-Signature-256` is a GitHub delivery; otherwise one with `X-Gitlab-Token` is a GitLab delivery; otherwise `401`. A request with both is a GitHub delivery, and its GitLab token is not looked at.
3. GitLab: the token header must appear exactly once and equal the secret, else `401`.
4. A body larger than 1 MiB (1,048,576 bytes): `413 payload_too_large`, whether or not the request declares its length. A body of exactly 1 MiB is accepted. For GitHub this is answered whatever the signature, because the body is not read past the limit.
5. GitHub: the signature header must appear exactly once and equal `sha256=` followed by the lower-case hex HMAC-SHA256 of the raw body under the secret, else `401`. A delivery with only the legacy `X-Hub-Signature` header has no scheme and is `401`.
6. Identity. The delivery id is `X-GitHub-Delivery` on GitHub, and `Idempotency-Key` or else `X-Gitlab-Event-UUID` on GitLab. The event name is `X-GitHub-Event` or `X-Gitlab-Event`. A missing id, a missing event name, or an id that is not 1 to 255 printable ASCII characters: `400 invalid_request`; nothing is recorded.
7. The delivery is reduced and recorded. If the workspace already has a delivery with this id the answer is `202 {"outcome":"duplicate"}` and nothing else happens.
8. Outcome `malformed`: `400 invalid_request`. The delivery stays recorded.
9. Otherwise `202 {"outcome": "relayed"|"ignored"|"repository_mismatch"}`.

Secrets are compared in constant time (FRG-FR-037). Every `401` has `error_type` `unauthenticated` and the message `delivery signature missing or invalid`, and records nothing.

A `FORGE_WEBHOOK_SECRET` stored through the secrets API is not bound by the 16-to-255 rule: any non-empty value works at the ingress (§12.10).

### Delivery record

One record per accepted delivery (§12.3). Payloads and secrets are never stored.

| Field | Type | Meaning |
|---|---|---|
| `delivery_id` | string | the forge's id for the delivery; unique per workspace |
| `forge` | string | `github` or `gitlab` |
| `event` | string | the forge's event name, cut to 64 bytes |
| `action` | string | the payload's action, `""` when it has none |
| `target` | string or null | `origin`, `upstream`, or `null` when no repository matched |
| `outcome` | string | `relayed`, `ignored`, `repository_mismatch` or `malformed` |
| `relayed` | array of strings | the event types relayed, in order; `[]` unless `outcome` is `relayed` |
| `received_at` | timestamp | when the hub accepted it |

`duplicate` is only ever answered to the forge and never recorded. A redelivery of a `malformed` delivery is answered `202` with `duplicate`. The same id in two workspaces is two deliveries.

Recording a delivery and appending its relayed events to the event log is one atomic step, completed before the `202` (FRG-FR-039). It is not abandoned when the forge closes the connection early. Of several concurrent copies of one delivery exactly one is recorded and its events are written once; the others are answered `duplicate`.

### Reduction

Reduction turns a verified payload into `action`, `target`, an outcome and the events to relay (FRG-FR-038). The body must be one JSON object, else the outcome is `malformed`. The action is GitHub's `action` or GitLab's `object_attributes.action`. The repository path is GitHub's `repository.full_name` or GitLab's `project.path_with_namespace`; it is compared, ignoring case, with the origin repository (`owner/name`) and then, for a carry-patch workspace with an `upstream_url`, with the upstream repository. The first match gives `target`. Then the first matching row decides:

| Situation | Outcome |
|---|---|
| no mapping for the event and action | `ignored` |
| a field the mapping requires is missing or of the wrong type | `malformed` |
| the mapping yields no events | `ignored` |
| events, but the payload names no repository | `malformed` |
| events, but the repository is neither origin nor upstream | `repository_mismatch` |
| otherwise | `relayed` |

### Mapping

Anything not listed is ignored, for example GitHub `ping` and edited comments (FRG-FR-040).

| Forge | Event and action | Hub event | Required |
|---|---|---|---|
| GitHub | `issues`: `opened`, `edited`, `closed`, `reopened` | `parley.forge.issue.<action>` | `issue.number` |
| GitHub | `issues`: `labeled`, `unlabeled` | `parley.forge.issue.<action>` | `issue.number`, `label.name` |
| GitHub | `issue_comment`: `created` | `parley.forge.comment.created` | `issue.number` |
| GitHub | `pull_request`: `opened`, `reopened`, `synchronize` | `parley.forge.pull_request.<action>` | `pull_request.number` |
| GitHub | `pull_request`: `closed` | `.merged` when `pull_request.merged` is true, else `.closed` | `pull_request.number` |
| GitHub | `push` | `parley.forge.push` | `ref` |
| GitLab | `Issue Hook`: `open`, `close`, `reopen` | `parley.forge.issue.opened`, `.closed`, `.reopened` | `object_attributes.iid` |
| GitLab | `Issue Hook`: `update` | `.edited` when the title or description changed; then one `.unlabeled` per removed label and one `.labeled` per added label, each group in name order | `object_attributes.iid` |
| GitLab | `Note Hook` on an issue or a merge request | `parley.forge.comment.created` | the `iid` of the issue or merge request |
| GitLab | `Merge Request Hook`: `open`, `close`, `reopen`, `merge` | `parley.forge.pull_request.opened`, `.closed`, `.reopened`, `.merged` | `object_attributes.iid` |
| GitLab | `Merge Request Hook`: `update` | `.synchronize` when new commits were pushed (`oldrev` present), else ignored | `object_attributes.iid` |
| GitLab | `Push Hook`, `Tag Push Hook` | `parley.forge.push` | `ref` |

One delivery relays at most 100 label events. A merged pull request yields `.merged` and never also `.closed`.

### Delivery events

Every delivery event (§12.8) has `workspace` the slug, `actor_type` `forge`, `actor_id` null, and `action` the last segment of its type. Common metadata: `forge`, `target`, `delivery_id` and `actor` (the forge login that caused the delivery, `""` when the payload names none).

| Event | `resource_type` | `resource_id` | Extra metadata |
|---|---|---|---|
| `parley.forge.issue.opened`, `.edited`, `.closed`, `.reopened` | `issue` | number | `number`, `title` (cut to 256 bytes), `url`, `labels` (at most 100 names) |
| `parley.forge.issue.labeled`, `.unlabeled` | `issue` | number | `number`, `label`, `labels` (the current set, at most 100), `url` |
| `parley.forge.comment.created` | `comment` | issue or pull request number | `number`, `comment_url`, `pull_request` (boolean) |
| `parley.forge.pull_request.opened`, `.closed`, `.merged`, `.reopened`, `.synchronize` | `pull_request` | number | `number`, `url`, `head`, `base`, `merged` (boolean) |
| `parley.forge.push` | `ref` | the ref | `ref`, `before`, `after` |

No delivery event carries a body, a comment text or a webhook secret, and no URL in one carries userinfo. `parley.forge.push` reports a push the forge received; it is unrelated to `parley.git.push`.

### Delivery log

`GET /api/v1/workspaces/:slug/forge/deliveries` needs `forge:read` and read access and works in any workspace state. It lists the recorded deliveries, newest first, by the list convention (`items`, `next_cursor`, `limit`, `cursor`). `since` (RFC 3339) keeps only deliveries received at or after it; a bad value is `400 invalid_request`. On a task fork the route answers `400 fork_has_no_origin`.

Deliveries are kept for 7 days (FRG-FR-041). The maintenance loop deletes older ones, once at start and then every hour; a failure is logged and retried at the next pass (§12.7). Deleting a workspace deletes its deliveries in the same step, so a slug that is used again starts with an empty log. The retention is fixed and not configurable.

The diagnostic route of package 41 reports `webhook: {"configured": boolean, "url": string|null, "last_delivery_at": timestamp|null}`. Package 41 computes `configured` (the secret is present and not empty) and `url` (the webhook URL above); this package supplies `last_delivery_at` through the provider it registers with package 41's forge component.

### CLI

- `parley forge webhook set <workspace-slug>` calls the `PUT` route. Flags, mutually exclusive: `--secret <string>`; `--from-stdin` (reads the secret from stdin and drops one trailing line break); `--generate` (the default). It prints the hub's answer. When the answer contains `secret`, a note on stderr says that it will not be shown again. Giving more than one of the flags is a usage error (exit 2) and nothing is sent.
- `parley forge webhook unset <workspace-slug>` calls the `DELETE` route and prints `{"status":"unset","workspace":"<slug>"}`.
- `parley forge deliveries <workspace-slug>` calls the list route. Flags: `--since <rfc3339>`, `--limit`, `--cursor`; a flag that is not given sends nothing.

### End-to-end flows

1. **Set, deliver, redeliver, read.** A developer runs `parley forge webhook set demo`; the CLI sends the `PUT`; `parleyd` generates and stores the secret and answers with the URL and the secret; the developer enters both on the forge. The forge posts a signed `issues` `labeled` delivery to `/hooks/forge/demo`; `parleyd` verifies the signature, reduces the payload, records the delivery and appends `parley.forge.issue.labeled` in one transaction and answers `202 relayed`. The forge sends the same delivery again and is answered `202 duplicate`. The developer runs `parley forge deliveries demo` and sees one delivery; the event log holds one event.
2. **GitLab label update, then unset.** The forge posts an `Issue Hook` `update` with the token header, a changed title, one removed and two added labels; `parleyd` records the delivery and appends `.edited`, one `.unlabeled` and two `.labeled` events in that order. The developer runs `parley forge webhook unset demo`; the next delivery from the forge is answered `404`, and the recorded delivery is still listed.

## Design Decisions

Project-wide decisions relied on: the store is embedded SQLite behind `internal/store` (4.2); HTTP is `net/http` (4.3); the CLI uses cobra through package 09 (4.4); tests use `testing`, `net/http/httptest` and a replaceable clock (4.7).

1. **Dependencies beyond the plan.** Package 09 is added because this package specifies three `parley` commands. Package 14 is added because this package registers a remover with the removers of owned data that package 14 creates in `internal/workspace`. Package 39 is reached through package 41.
2. **Check order on the `PUT`.** After scope and access: the fork refusal, then the body, then the archived refusal, then `503 secrets_unavailable`, then `429 secret_limit_reached`. Why: the PRD orders body before archived (FRG-FR-002) and does not place the other three; a route that does not apply to a fork says so before it reads the body, and a hub that cannot store any secret says so before it counts them. Where the write routes of package 42 have the same step, the order is the same: scope, access (`403 forbidden` for read access only, also on an archived workspace), body, `409 workspace_archived`. The routes are registered with package 02's route registration and the access component, not with the forge route helper of package 41, because that helper switches a fork to its parent and these routes refuse a fork. The `503` is the HTTP error package 39 exports for `ErrUnavailable`; the `429` carries `error_type` `secret_limit_reached` and the message `maximum of 100 entries per scope exceeded`, as on the secrets routes.
3. **An existing secret under another casing is replaced.** The secret is stored with package 39's `Put`, an upsert inside the caller's transaction that writes no secret event: secret keys match ignoring case (§11), so a workspace that holds `forge_webhook_secret` keeps one entry, with its stored casing, and the limit of 100 is checked only when a new entry would be created (`Put` returns `ErrLimitReached`). The `DELETE` removes it with package 39's `Delete`, which reports whether an entry existed.
4. **`DELETE` always records its event**, with `existed` true or false. Why: the event table gives `existed` as a boolean, which has a meaning only if the event is also written when nothing existed.
5. **The `404` body of the ingress** is the error envelope with message `not found`, the same for the three cases.
6. **Bodies and content type at the ingress.** The content type is not checked and never answers `415`: §12.10 requires a signed form-encoded delivery to be recorded as `malformed`. An empty body is not a JSON object and is `malformed`.
7. **Delivery id characters.** "Printable ASCII" for the id is the range space to `~` (0x20 to 0x7E). When a header that carries the id or the event name appears several times, its first value is used.
8. **`event` and `action` are cut to 64 bytes** at a UTF-8 character boundary. The PRD states the cut for `event`; `action` gets the same so that a record has a bounded size. A non-string action is `""`.
9. **An unreadable secret at the ingress answers `500 internal`** and records nothing. Why: §11 says a failure to read or decrypt a secret fails the operation and is never treated as "no credential", so it must not become `404` or `401`.
10. **A failure to store the delivery or its events answers `500 internal`** with nothing stored, so the forge's redelivery is processed as new. The management routes follow the same rule, as the write routes of package 42 do: when the event of a `PUT` or a `DELETE` cannot be recorded, the transaction is rolled back, the secret is as it was and the answer is `500 internal`. Events are appended with `Emit` of package 52 inside this package's transaction.
11. **Required fields.** A required number (`issue.number`, `pull_request.number`, an `iid`) is a JSON integer of at least 1; a required string (`label.name`, `ref`) is a non-empty JSON string. Anything else is "missing or of the wrong type". Why: the event's `resource_id` is never empty (§14).
12. **Where metadata is read from.** The PRD names the metadata, not the payload fields. GitHub: `actor` `sender.login`; `title` `issue.title`; `url` `issue.html_url` or `pull_request.html_url`; `labels` the `name` of each entry of `issue.labels`; `label` `label.name`; `comment_url` `comment.html_url`; `pull_request` (boolean) whether `issue.pull_request` is an object; `head` `pull_request.head.ref`; `base` `pull_request.base.ref`; `ref`, `before`, `after` the top-level members. GitLab: `actor` `user.username`, or `user_username` on push hooks; `title` `object_attributes.title`; `url` `object_attributes.url`; `labels` the `title` of each entry of the top-level `labels`; `comment_url` `object_attributes.url` of the note; `head` `object_attributes.source_branch`; `base` `object_attributes.target_branch`; `ref`, `before`, `after` the top-level members. An optional value that is missing or of the wrong type is `""` (a string), `[]` (a list) or `false` (a boolean); it never makes a delivery `malformed`. URLs lose their userinfo.
13. **`merged` in pull request metadata** is `true` exactly on `parley.forge.pull_request.merged` and `false` on the other four types.
14. **GitLab `Issue Hook` `update`.** "Title or description changed" means `changes.title` or `changes.description` is present. Removed labels are the titles in `changes.labels.previous` that are not in `changes.labels.current`; added labels are the reverse. Names are ordered by their bytes. The limit of 100 label events keeps the first 100 in emission order (unlabeled before labeled). An update that changes none of these yields no events and is `ignored`.
15. **GitLab `Note Hook`.** The note's kind is `object_attributes.noteable_type`: `Issue` reads `issue.iid`, `MergeRequest` reads `merge_request.iid`, any other kind is ignored. The action of a note is not looked at, because the PRD's row names none.
16. **GitLab `Merge Request Hook` `update`.** "`oldrev` present" means `object_attributes.oldrev` is a non-empty string.
17. **Order of the list.** `received_at` descending, and for equal instants the later recorded first. `target` and other unknown query parameters are ignored on the deliveries and webhook routes, which take no `target` (FRG-FR-003).
18. **Pruning boundary.** A pass deletes the deliveries whose `received_at` is more than 7 days (604,800 seconds) before the pass. A pruned id that the forge sends again is a new delivery.
19. **`webhook.last_delivery_at`** is the `received_at` of the newest recorded delivery of the workspace, `null` when none is recorded; it therefore becomes `null` again once all deliveries are pruned. This package registers it with package 41's forge component through `SetLastDeliveryProvider`: a function of a context and a workspace slug that returns the time, whether there is one, and an error. `configured` and `url` are computed by package 41's handler, which this package does not change; `configured` is true when the secret is present and not empty, the test the ingress applies.
20. **`parley forge webhook set` with `--generate`** sends no body. `--secret` and `--from-stdin` send `{"secret": "<value>"}`; the hub, not the CLI, checks the value.

## Dependencies

- **41 forge_reads** (draft): the repository (`owner`, `name`) derived from the origin and upstream URLs, the diagnostic route that reports the `webhook` member, the `forge` command group of the CLI; through it, package 39 secrets, whose component stores and reads `FORGE_WEBHOOK_SECRET`.
- **52 durable_event_log_and_stream** (draft): appends `parley.forge.webhook.*` and the delivery events inside the caller's transaction.
- **09 cli_foundation** (draft): the HTTP client, output and exit codes the three commands are built on.
- **14 workspace_archive_reclone_delete** (draft): the removers of owned data that the workspace delete runs inside its transaction; this package registers the remover of a workspace's deliveries there.
- Plugged into at start, not edited: the maintenance task registry of package 03 (task `forge_delivery_pruning`), the route registration and rate limiter of package 02.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Event webhook**: A workspace's subscription that makes the hub send each matching hub event to a URL as a signed HTTP `POST`. Not to be confused with the forge's webhooks, which the hub receives.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Webhook secret**: The workspace-scope secret `FORGE_WEBHOOK_SECRET`, read on every delivery.
- **Delivery**: One record per accepted webhook delivery of a forge.
- **Reduction**: Turning a verified payload into an action, a target, an outcome and the hub events to relay.
