[Parley PRD](00_overview.md) · §12

## 12. Forge integration

### 12.1 Purpose and scope

A workspace's repository usually lives on a forge: GitHub or GitLab. The hub is the forge client of each workspace. Callers read and write the issues, labels, comments and pull requests (GitLab: merge requests) of that repository **through the hub**, with the credential the hub stores for the workspace. The caller never holds a forge token. The hub also receives the forge's webhook deliveries for a workspace, verifies them against a per-workspace secret, records each one once and relays it as `parley.forge.*` events.

**Relation to changes.** Parley's own unit of review and landing is the change (§7, §8). The forge remains the system of record for issues, and for teams that review on pull requests. The two are independent:

- The hub never acts on a forge event. A delivery does not open, update, approve or land a change, and it does not trigger a sync.
- A change is not a pull request and does not create one. A pull request opened through this section is an ordinary forge pull request between branches that exist on the forge.
- Forge objects carry no reference to changes, and changes carry no reference to forge objects. A caller that wants to connect them does so in text (a title, an intent, a comment).

Out of scope: pushing branches to origin (§4); registering the webhook on the forge (the hub never does this; a person enters the URL and the secret in the forge's settings); forges other than GitHub and GitLab.

### 12.2 User stories

- **FRG-US-01** As an agent with a `forge:read` token, I want to list and read the issues and pull requests of my workspace's repository, so that I can triage without a forge credential.
- **FRG-US-02** As an agent with `forge:write`, I want to create issues, comment, label and open pull requests with the workspace's stored credential. Every write that reached the forge leaves exactly one event, whether it succeeded or failed.
- **FRG-US-03** As a developer, I want to set a webhook secret, generated or my own, and get the URL to enter on the forge. A generated secret is shown once.
- **FRG-US-04** As the forge, I post deliveries to `/hooks/forge/:slug`. A valid delivery is acknowledged quickly and relayed once. A redelivery is answered `duplicate`.
- **FRG-US-05** As an org member with read-only access, I can read the forge through the workspace's credential but not write.
- **FRG-US-06** As a carry-patch developer, I want `--target upstream` to read and write the upstream repository with the upstream credential.
- **FRG-US-07** As an agent working in a task fork, I want to read the parent's issues through my fork's slug, so that I do not need to know the parent's forge.

### 12.3 Concepts and data model

**Remote target.** The query parameter `target` selects the repository a route works on.

| Value | Remote | Credential | API URL setting |
|---|---|---|---|
| absent or `origin` | the workspace's `git_url` | the workspace's origin git credential (§4, §11) | workspace setting `forge_api_url` |
| `upstream` | the workspace's `upstream_url`; only for a carry-patch workspace (§10) that has one | the workspace's upstream git credential, which falls back to the origin credential (§10) | workspace setting `upstream_forge_api_url` (no fallback to `forge_api_url`) |

**Repository.** The hub derives `host`, `owner` and `name` from the remote URL. It accepts `https://`, `http://`, `ssh://` and `git://` URLs and the scp form `user@host:owner/name`. The host is lower-cased, a leading `www.` and any port are dropped, userinfo is ignored, and a trailing `.git` is removed. The path needs at least two segments: `name` is the last one, `owner` is everything before it (GitLab subgroups are kept: `group/subgroup`).

**Forge kind.** `github` or `gitlab`, decided in this order:

1. If the API URL setting is set and contains exactly one of `github` and `gitlab`, that forge.
2. Otherwise, if the remote's host contains exactly one of `github` and `gitlab`, that forge.
3. Otherwise the forge is unknown.

A self-hosted forge whose host name names neither product is made known by setting the API URL.

**API URL.** The setting, when set, with a trailing `/` removed. Otherwise the default for the host: `https://api.github.com` for `github.com`, `https://<host>/api/v3` for another GitHub host, `https://<host>/api/v4` for GitLab. A GitLab API URL that does not end in `/api/v4` gets it appended. The setting must be an absolute `https` URL with a host and without userinfo, query or fragment; §4 validates it when it is stored.

**Credential kind.** What the hub sends to the forge, derived from the workspace's git credential for the target:

| Kind | Stored credential | Sent to the forge |
|---|---|---|
| `pat` | an access token | the token |
| `basic` | a username and a password | the password, as the token |
| `none` | nothing | no authentication |

The hub sends only a credential stored for the workspace. It never reads a forge token from its own process environment, neither to authenticate nor to decide whether it is authenticated.

**Result types.** Timestamps follow §3; an absent timestamp is `null`. `url` is always the web URL of the object.

| Type | Fields |
|---|---|
| repository | `full_name`, `default_branch`, `private`, `archived`, `permissions {pull, push, admin}`, `allow_merge_commit`, `allow_squash_merge`, `allow_rebase_merge` (the last three `null` when the forge does not report them) |
| issue | `number`, `title`, `body`, `state` (`open`, `closed`), `url`, `author {login}`, `labels` (names), `created_at`, `updated_at`, `closed_at`, `is_pr` |
| issue thread | `issue`, `comments` (the first page, oldest first), `comments_next_cursor`, `comments_error` |
| comment | `id`, `body`, `author {login}`, `created_at`, `updated_at`, `url` |
| pull request | `number`, `title`, `body`, `state` (`open`, `closed`, `merged`), `url`, `draft`, `merged`, `head_sha`, `head_branch`, `base_branch`, `author {login}`, `created_at`, `updated_at`, `closed_at`, `merged_at` |
| pull request state | `number`, `state`, `merged`, `head_sha` |
| changed file | `filename`, `status` (`added`, `modified`, `deleted`, `renamed`), `additions`, `deletions` |
| check run | `name`, `status` (`queued`, `in_progress`, `completed`), `conclusion` (`success`, `failure`, `cancelled`, `skipped`, `neutral`, or `null` while not completed), `summary`, `url` |
| review | `author {login}`, `state` (`approved`, `changes_requested`, `commented`, `dismissed`), `body`, `submitted_at` |
| label | `name`, `color` (six lower-case hex digits, no `#`), `description` |

On GitLab, a number is the project-level `iid`; check runs are the jobs of the merge request's latest pipeline; reviews are the approvals (as `approved`) and the non-system notes (as `commented`), ordered by time.

**Webhook secret.** The workspace-scope secret `FORGE_WEBHOOK_SECRET` (§11). It is read on every delivery, so a rotation applies to the next delivery. The **webhook URL** is `server.external_url` without a trailing `/`, followed by `/hooks/forge/<slug>`; it is `null` when `external_url` is empty.

**Delivery.** One record per accepted webhook delivery. Payloads and secrets are never stored.

| Field | Type | Meaning |
|---|---|---|
| `delivery_id` | string | the forge's id for the delivery; unique per workspace |
| `forge` | string | `github` or `gitlab` |
| `event` | string | the forge's event name, cut to 64 bytes |
| `action` | string | the payload's action, `""` when it has none |
| `target` | string or null | `origin`, `upstream`, or `null` when no repository matched |
| `outcome` | string | `relayed`, `ignored`, `repository_mismatch` or `malformed` |
| `relayed` | array of strings | the event types relayed; `[]` unless `outcome` is `relayed` |
| `received_at` | timestamp | when the hub accepted it |

A fifth outcome, `duplicate`, is only ever answered to the forge. It is never recorded.

### 12.4 Functional requirements

**Access and order of checks**

- **FRG-FR-001** Read routes need `forge:read`; write routes and the webhook `PUT` and `DELETE` need `forge:write` (§3). Read routes need read access to the workspace; write and webhook routes need write access, so a read-only org member gets `403 forbidden`.
- **FRG-FR-002** Every forge route follows the check order of §3. Within it, the steps specific to this section run in this order: (1) `target`; (2) path and query parameters and the request body, as far as they do not depend on the forge kind; (3) the archived check; (4) forge configuration (FRG-FR-005); (5) checks that depend on the forge kind (FRG-FR-017, FRG-FR-024); (6) for writes, the credential check (FRG-FR-025); (7) the call. A request refused before step 7 sends nothing to the forge and records no event.
- **FRG-FR-003** `target` may be given at most once. `origin` or absent selects origin. `upstream` selects upstream when the workspace is a carry-patch workspace with an `upstream_url`; otherwise the answer is `400`, `error_type: workspace_mode_mismatch`. Any other value is `400 invalid_request`. The diagnostic route, the deliveries route and the webhook routes take no `target`.
- **FRG-FR-004** Writes and the webhook `PUT` are refused on an archived workspace with the archived-workspace error of §4 (`409` `workspace_archived`). A write through a task fork whose parent is archived is refused with `409` `parent_archived` (WS-FR-066). Reads, the deliveries list and the webhook `DELETE` work in any workspace state. No forge route needs the hub's copy of the repository, and none is affected by `workspace_busy`.
- **FRG-FR-005** Forge configuration is resolved in a fixed order, and the first failure wins: the API URL setting is not a valid URL → `422`, `error_type: forge_api_url_invalid`; the remote URL names no repository → `422`, `error_type: forge_unknown`; the forge kind is unknown → `422`, `error_type: forge_unknown`, with a message that names the setting to set.
- **FRG-FR-006** A `:number` path parameter is a positive decimal integer: digits only, no sign, no leading zero, at most 2,147,483,647. Anything else is `400 invalid_request` with the message `number must be a positive decimal integer`. The rule is the same on every route, read or write, and in the CLI.
- **FRG-FR-007** **Task forks.** A task fork (§4) has no forge of its own. Every forge route called with a fork's slug, except the webhook and deliveries routes, acts on the parent's forge repository with the parent's credential and the parent's settings, including `target=upstream`. The caller must have, on the parent, the access the route needs: read access for reads, write access for writes. A caller without it gets `403 forbidden`. A token bound to the fork is accepted for this purpose; the access that counts is that of the user behind it. The webhook `PUT` and `DELETE` and the deliveries list are refused on a fork with `400`, `error_type: fork_has_no_origin`.

**Outbound calls**

- **FRG-FR-008** Every forge request carries `User-Agent: parleyd/<version>` and honours the proxy variables of §12.9. A redirect is followed only to the same scheme, host and port, at most 10 times. Each request has a 30-second deadline. A response is read up to 8 MiB.
- **FRG-FR-009** A rate-limited request is retried once when the wait the forge states fits inside the request's deadline. Otherwise the caller gets `429` (FRG-FR-029). The rule is the same for every request the hub sends.

**Reads**

- **FRG-FR-010** A read without a stored credential is sent anonymously. Reads record no events.
- **FRG-FR-011** Lists follow the list convention of §3 (`items`, `next_cursor`, `limit`, `cursor`). A cursor is opaque and valid only for the same route with the same parameters; any other use is `400 invalid_request`.
- **FRG-FR-012** The issue list takes `state` (`open`, `closed`, `all`; default `open`), `sort` (`created`, `updated`; default `created`), `direction` (`asc`, `desc`; default `desc`), `labels` (comma-separated names; an issue must carry all of them) and `assignee` (a login). Each may be given at most once; a repeated parameter or a value outside the set is `400`. Pull requests are never part of the issue list.
- **FRG-FR-013** The issue thread is the issue plus its first page of comments (at most 100). `comments_next_cursor` continues on the comments route and is `null` when the page is complete. If the issue was read but its comments were not, the answer is still `200`, `comments` is `[]`, and `comments_error` is `{"error_type","message"}` with the values FRG-FR-029 would have given. Otherwise `comments_error` is `null`.

**Writes**

- **FRG-FR-014** A write body is a JSON object unless stated. An absent field and a `null` field both mean "not set". Unknown fields are ignored. A required string must not be blank. A list of names has at most 100 entries, none blank. A violation is `400 invalid_request` and names the field.
- **FRG-FR-015** **Create issue.** `title` required; `body`, `labels`, `assignees` optional. Answers `201` with the issue.
- **FRG-FR-016** **Update issue.** Any of `title` (not blank), `body` (`""` clears it), `state` (`open` or `closed`), `labels` and `assignees` (each replaces the whole set; `[]` clears it). No field set → `400`. Answers `200` with the issue as the forge returns it.
- **FRG-FR-017** **GitLab assignees.** GitLab takes assignees as user ids, which the hub does not resolve. On GitLab, `assignees` is not sent on create or update. An update whose only field is `assignees` is refused with `400 invalid_request`.
- **FRG-FR-018** **Close issue.** The body may be empty or `{"comment": string}`. A non-blank comment is posted first; the issue is closed only if the comment succeeded. If the comment fails, the answer is that failure and the issue is untouched. If the close fails after the comment was posted, the answer is the close's failure and the event carries `comment_url`. Closing a closed issue succeeds. Answers `200` with the issue.
- **FRG-FR-019** **Comment** on an issue or a pull request. `body` required. Answers `201` with `{"url": "<web URL of the comment>"}`.
- **FRG-FR-020** **Add labels.** The body is a bare JSON array of 1 to 100 non-blank names. The labels are added and the issue keeps its others. Answers `204`.
- **FRG-FR-021** **Remove label.** The label is the last path segment, percent-decoded once, so `type%2Fbug` names `type/bug`. A blank or undecodable name is `400`. The route is idempotent: it answers `204` when the issue exists, whether or not it carried the label, and `404 forge_not_found` when the issue does not exist.
- **FRG-FR-022** **Ensure label.** `name` required; `color` optional, six hex digits with or without a leading `#`, default `ededed`; `description` optional. If the repository has a label of that name, the answer is `200` with the label as the forge holds it, unchanged. Otherwise the label is created and the answer is `201`. If two callers race, the loser answers `200` with the winner's label.
- **FRG-FR-023** **Open pull request.** `title`, `head` and `base` required; `body` optional; `draft` optional, default `false`. `head` is sent as given, so `owner:branch` names a branch of a forge-side fork. Both branches must exist on the forge (§4). A pull request that already exists for the same head and base is the forge's refusal (`422 forge_rejected`). Answers `201` with the pull request.
- **FRG-FR-024** **Body limits.** Long text is refused before anything is sent: 65,536 characters (Unicode code points) on GitHub, 1,000,000 on GitLab. The measured fields are the `body` of an issue, a comment or a pull request, and a close's `comment`. Over the limit → `413 payload_too_large`, with a message that names the field, its length and the limit.
- **FRG-FR-025** **Missing credential.** When the credential kind for the target is `none`, every write route is refused with `422`, `error_type: forge_credential_missing`. Nothing is sent to the forge, including the lookups that some writes begin with, and no event is recorded.
- **FRG-FR-026** A write is not abandoned when the caller disconnects. It runs to its end and its event is recorded.
- **FRG-FR-027** The `POST` routes that create something (an issue, a comment, a pull request, a label) accept `Idempotency-Key` as §3 specifies. A replay sends nothing to the forge and records no event.
- **FRG-FR-028** **Events.** A write route records exactly one `parley.forge.*` event when it sent at least one request to the forge, whether the write succeeded or failed. The event is recorded before the response is sent.

**Error mapping**

- **FRG-FR-029** A failed forge call maps as follows; the first matching row wins.

| Condition | Status | `error_type` |
|---|---|---|
| redirect to another host or scheme, or more than 10 redirects | 502 | `forge_error` |
| no answer within the deadline | 504 | `forge_timeout` |
| rate limited, after the retry of FRG-FR-009 | 429 | `forge_rate_limited` |
| the forge answers 404 | 404 | `forge_not_found` |
| the forge answers 401 | 502 | `forge_unauthenticated` |
| the forge answers 403 (not a rate limit) | 502 | `forge_forbidden` |
| the forge answers any other 4xx | 422 | `forge_rejected` |
| the forge answers 5xx or something the hub cannot read | 502 | `forge_error` |
| the connection fails (DNS, TCP, TLS) | 502 | `forge_unreachable` |

- **FRG-FR-030** The message is `the forge answered <status>: <the forge's message>`, or a plain description when the forge gave no answer. A `429` carries a `Retry-After` header in whole seconds when the forge stated a wait.
- **FRG-FR-031** Every message is scrubbed before it leaves the hub: the workspace's token is replaced by `[redacted]`, URL userinfo is removed, and the text is cut to 1,024 bytes. No response, event or log line carries a forge credential.

**Webhook management**

- **FRG-FR-032** `PUT …/forge/webhook` stores the webhook secret. With no body, `{}` or `{"secret": null}`, the hub generates one: 32 random bytes as 64 lower-case hex characters. A given `secret` must be a string of 16 to 255 printable ASCII characters without spaces (`!` to `~`); otherwise `400 invalid_request`. An error message never contains the value. A generated secret is returned in this one response and never again.
- **FRG-FR-033** `DELETE …/forge/webhook` removes the secret and answers `204` whether or not one existed. From then on the ingress answers `404` for the workspace. Recorded deliveries remain.
- **FRG-FR-034** The hub never registers, changes or deletes a webhook on the forge.

**Ingress**

- **FRG-FR-035** `POST /hooks/forge/:slug` is public: it takes no bearer credential, and the delivery's signature or token is its authentication. It never contacts the forge. An archived workspace still relays.
- **FRG-FR-036** The ingress decides in this order:
  1. Unknown slug, a task fork's slug, or a workspace without a webhook secret → `404 not_found`, with one identical body for all three.
  2. Scheme: a request with `X-Hub-Signature-256` is a GitHub delivery; otherwise one with `X-Gitlab-Token` is a GitLab delivery; otherwise `401`.
  3. GitLab: the token header must appear exactly once and equal the secret, else `401`.
  4. Body larger than 1 MiB → `413 payload_too_large`.
  5. GitHub: the signature header must appear exactly once and equal `sha256=` followed by the lower-case hex HMAC-SHA256 of the raw body under the secret, else `401`.
  6. Identity: the delivery id is `X-GitHub-Delivery` on GitHub, and `Idempotency-Key` or else `X-Gitlab-Event-UUID` on GitLab. The event name is `X-GitHub-Event` or `X-Gitlab-Event`. A missing id, a missing event name, or an id that is not 1 to 255 printable ASCII characters → `400 invalid_request`; nothing is recorded.
  7. The delivery is reduced (FRG-FR-037) and recorded. If the workspace already has a delivery with this id, the answer is `202 {"outcome":"duplicate"}` and nothing else happens.
  8. Outcome `malformed` → `400 invalid_request`. The delivery stays recorded.
  9. Otherwise `202 {"outcome": …}`.
- **FRG-FR-037** Secrets are compared in constant time. Every `401` has the same message, `delivery signature missing or invalid`, with `error_type: unauthenticated`, and records nothing.
- **FRG-FR-038** **Reduction.** The body must be one JSON object, else `malformed`. The hub reads the action (GitHub `action`; GitLab `object_attributes.action`) and the repository path (GitHub `repository.full_name`; GitLab `project.path_with_namespace`). The path is compared, ignoring case, with the origin repository and then, for a carry-patch workspace, with the upstream repository; this gives `target`. Then:

| Situation | Outcome |
|---|---|
| no mapping for the event and action (FRG-FR-040) | `ignored` |
| a field the mapping requires is missing or of the wrong type | `malformed` |
| the mapping yields no events | `ignored` |
| events, but the payload names no repository | `malformed` |
| events, but the repository is neither origin nor upstream | `repository_mismatch` |
| otherwise | `relayed` |

- **FRG-FR-039** Recording a delivery and appending its relayed events to the event log (§14) is one atomic step, completed before the `202`. A delivery recorded as `relayed` therefore always has its events, even if the forge closed the connection early. Of several concurrent copies of one delivery, exactly one is recorded; the others are answered `duplicate`.
- **FRG-FR-040** **Mapping.** Anything not listed is ignored, for example GitHub `ping` and edited comments.

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

One delivery relays at most 100 label events.

**Delivery log**

- **FRG-FR-041** Deliveries are kept for 7 days. Deleting a workspace deletes its deliveries in the same step, so a slug that is used again starts with an empty log.

### 12.5 REST API

All routes except the ingress are under `/api/v1`, use the error envelope and check order of §3, and follow FRG-FR-002. Unless a route says otherwise:

- A **read** route needs `forge:read` and read access, takes `target`, and fails with the forge errors: the `422` configuration errors of FRG-FR-005 and the mapped failures of FRG-FR-029.
- A **list** route is a read route that also takes `limit` and `cursor` and answers a list (§3).
- A **write** route needs `forge:write` and write access and takes `target`. It fails with the forge errors, the archived refusal, `413 payload_too_large` where text is measured, and `422 forge_credential_missing`.

#### `GET /workspaces/:slug/forge`
Diagnostic: says how the hub reaches the forge, without contacting it. Needs `forge:read` and read access; takes no `target`. Answers `200`:
```json
{"forge": "github",
 "api_url": "https://api.github.com",
 "repository": {"host": "github.com", "owner": "acme", "name": "widgets"},
 "credential": "pat",
 "upstream": null,
 "webhook": {"configured": true,
             "url": "https://hub.example.com/hooks/forge/widgets",
             "last_delivery_at": "2026-10-21T09:12:03Z"}}
```
`forge` is `github`, `gitlab` or `unknown`. A remote that names no repository or an undetectable forge is reported (`forge: "unknown"`, `api_url: null`, and `repository: null` when there is none); it is not an error here. `upstream` has the same four fields for a carry-patch workspace with an `upstream_url`, else `null`. `webhook.url` and `webhook.last_delivery_at` may be `null`. For a task fork the whole document describes the parent. The only forge error is `422 forge_api_url_invalid`.

#### `GET /workspaces/:slug/forge/repository`
Read route. Answers `200` with the repository as the forge reports it.

#### `GET /workspaces/:slug/issues`
List route, with the filters `state`, `sort`, `direction`, `labels` and `assignee` (FRG-FR-012). Answers a list of issues.

#### `GET /workspaces/:slug/issues/:number`
Read route. Answers `200` with the issue thread (FRG-FR-013). On GitHub, the number of a pull request answers the issue with `is_pr: true`.

#### `GET /workspaces/:slug/issues/:number/comments`
List route. Answers the issue's comments, oldest first.

#### `GET /workspaces/:slug/pulls/:number`
Read route. Answers `200` with the pull request.

#### `GET /workspaces/:slug/pulls/:number/state`
Read route; the short form for polling. Answers `200` with the pull request state.

#### `GET /workspaces/:slug/pulls/:number/files`
List route. Answers the files the pull request changes.

#### `GET /workspaces/:slug/pulls/:number/checks`
List route. Answers the check runs of the pull request's head; empty when it has none.

#### `GET /workspaces/:slug/pulls/:number/reviews`
List route. Answers the pull request's reviews, oldest first.

#### `POST /workspaces/:slug/issues`
Write route (FRG-FR-015). Body: `{"title": string, "body"?: string, "labels"?: [string], "assignees"?: [string]}`. Answers `201` with the issue. Records `parley.forge.issue.create`.

#### `PATCH /workspaces/:slug/issues/:number`
Write route (FRG-FR-016, FRG-FR-017). Body: `{"title"?: string, "body"?: string, "state"?: "open"|"closed", "labels"?: [string], "assignees"?: [string]}`. Answers `200` with the issue. Records `parley.forge.issue.update`.

#### `POST /workspaces/:slug/issues/:number/close`
Write route (FRG-FR-018). Body: empty or `{"comment"?: string}`. Answers `200` with the issue. Records `parley.forge.issue.close`.

#### `POST /workspaces/:slug/issues/:number/comments`
Write route (FRG-FR-019). Body: `{"body": string}`. Answers `201` with `{"url": string}`. Records `parley.forge.comment.create`.

#### `POST /workspaces/:slug/issues/:number/labels`
Write route (FRG-FR-020). Body: a bare array `["name", …]`. Answers `204`. Records `parley.forge.label.add`.

#### `DELETE /workspaces/:slug/issues/:number/labels/:name`
Write route (FRG-FR-021). Answers `204`, or `404 forge_not_found` when the issue does not exist. Records `parley.forge.label.remove`.

#### `POST /workspaces/:slug/labels`
Write route (FRG-FR-022). Body: `{"name": string, "color"?: string, "description"?: string}`. Answers `200` with the existing label or `201` with the created one. Records `parley.forge.label.create` in both cases.

#### `POST /workspaces/:slug/pulls`
Write route (FRG-FR-023). Body: `{"title": string, "head": string, "base": string, "body"?: string, "draft"?: boolean}`. Answers `201` with the pull request. Records `parley.forge.pull_request.create`.

#### `POST /workspaces/:slug/pulls/:number/comments`
Write route (FRG-FR-019); the comment goes to the pull request's conversation. Body: `{"body": string}`. Answers `201` with `{"url": string}`. Records `parley.forge.pull_request.comment`.

#### `PUT /workspaces/:slug/forge/webhook`
Sets or rotates the webhook secret (FRG-FR-032). Needs `forge:write` and write access; refused on an archived workspace and on a task fork. Body: optional `{"secret"?: string}`. Answers `200` with `{"configured": true, "url": string|null, "secret"?: string}`; `secret` is present only when the hub generated it. `400` for a bad secret; `429 secret_limit_reached` when the workspace's secret scope is full (§11). `503 secrets_unavailable` when no encryption key is configured (§11). Records `parley.forge.webhook.set`.

#### `DELETE /workspaces/:slug/forge/webhook`
Removes the webhook secret (FRG-FR-033). Needs `forge:write` and write access; works in any workspace state; refused on a task fork. Answers `204`. Records `parley.forge.webhook.unset`.

#### `GET /workspaces/:slug/forge/deliveries`
Lists recorded deliveries (§12.3), newest first. Needs `forge:read` and read access; refused on a task fork. Query: `since` (RFC 3339; only deliveries received at or after it; a bad value is `400`), `limit`, `cursor`.
```json
{"items": [{"delivery_id": "72d3162e-cc78-11e3-81ab-4c9367dc0958", "forge": "github",
            "event": "issues", "action": "labeled", "target": "origin",
            "outcome": "relayed", "relayed": ["parley.forge.issue.labeled"],
            "received_at": "2026-10-21T09:12:03Z"}],
 "next_cursor": null}
```

#### `POST /hooks/forge/:slug`
Webhook ingress for the forge (FRG-FR-035 to FRG-FR-040). No bearer credential; the delivery's signature (GitHub) or token (GitLab) authenticates it. Body: the forge's payload, at most 1 MiB. Answers `202` with `{"outcome": "relayed"|"ignored"|"repository_mismatch"|"duplicate"}`. Errors: `404 not_found`, `401 unauthenticated`, `413 payload_too_large`, `400 invalid_request` (no delivery id, no event name, malformed payload). Side effects: a delivery record and the relayed events.

### 12.6 CLI

The conventions of §3 apply. Every command prints the hub's answer unless a different output is stated. Slugs, numbers and label names are sent as single percent-encoded path segments. A flag that is not given sends nothing, so the hub's defaults apply.

- `<number>` is checked with the rule of FRG-FR-006 before anything is sent; a violation is a usage error (exit 2).
- `--target <origin|upstream>` is available on every command that calls a route with a `target` parameter.
- `--limit <n>` and `--cursor <cursor>` are available on every list command.
- Text input: `--body <text>` gives the text; `--body-file <path>` reads it from a file, or from stdin when the path is `-`. The text is sent exactly as given. Giving both is a usage error.

#### `parley forge status <workspace-slug>`
Calls `GET /workspaces/:slug/forge`.

#### `parley forge repository <workspace-slug>`
Calls `GET /workspaces/:slug/forge/repository`. Flags: `--target`.

#### `parley forge webhook set <workspace-slug>`
Calls `PUT /workspaces/:slug/forge/webhook`. Flags, mutually exclusive: `--secret <string>`; `--from-stdin` (reads the secret from stdin and drops one trailing line break); `--generate` (the default). When the answer contains `secret`, a note on stderr says that it will not be shown again.

#### `parley forge webhook unset <workspace-slug>`
Calls `DELETE /workspaces/:slug/forge/webhook`. Prints `{"status":"unset","workspace":"<slug>"}`.

#### `parley forge deliveries <workspace-slug>`
Calls `GET /workspaces/:slug/forge/deliveries`. Flags: `--since <rfc3339>`, `--limit`, `--cursor`.

#### `parley issue list <workspace-slug>`
Calls `GET /workspaces/:slug/issues`. Flags: `--state`, `--label` (repeatable; sent as one `labels` parameter), `--assignee`, `--limit`, `--cursor`, `--target`.

#### `parley issue get <workspace-slug> <number>`
Calls `GET /workspaces/:slug/issues/:number`. Flags: `--target`.

#### `parley issue comments <workspace-slug> <number>`
Calls `GET /workspaces/:slug/issues/:number/comments`. Flags: `--limit`, `--cursor`, `--target`.

#### `parley issue create <workspace-slug>`
Calls `POST /workspaces/:slug/issues`. Flags: `--title` (required), `--body` or `--body-file`, `--label` (repeatable, one name each), `--assignee` (repeatable), `--target`.

#### `parley issue update <workspace-slug> <number>`
Calls `PATCH /workspaces/:slug/issues/:number` with only the given fields. Flags: `--title`, `--body` or `--body-file`, `--state`, `--label` (repeatable), `--assignee` (repeatable), `--target`. With no field flag it is a usage error. `--body ""` clears the body.

#### `parley issue close <workspace-slug> <number>`
Calls `POST /workspaces/:slug/issues/:number/close`. Flags: `--comment <text>`, `--target`.

#### `parley issue comment <workspace-slug> <number>`
Calls `POST /workspaces/:slug/issues/:number/comments`. Flags: `--body` or `--body-file` (one is required), `--target`.

#### `parley issue label <workspace-slug> <number> <label>...`
Calls `POST /workspaces/:slug/issues/:number/labels` with the labels. Flags: `--target`. Prints `{"status":"labeled","number":<n>,"labels":[…]}`.

#### `parley issue unlabel <workspace-slug> <number> <label>`
Calls `DELETE /workspaces/:slug/issues/:number/labels/:name`. Flags: `--target`. Prints `{"status":"unlabeled","number":<n>,"label":"<label>"}`.

#### `parley label create <workspace-slug> <name>`
Calls `POST /workspaces/:slug/labels`. Flags: `--color <hex>`, `--description <text>`, `--target`. Exit 0 whether the label existed or was created.

#### `parley pr get <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number`. Flags: `--target`.

#### `parley pr state <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number/state`. Flags: `--target`.

#### `parley pr files <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number/files`. Flags: `--limit`, `--cursor`, `--target`.

#### `parley pr checks <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number/checks`. Flags: `--limit`, `--cursor`, `--target`.

#### `parley pr reviews <workspace-slug> <number>`
Calls `GET /workspaces/:slug/pulls/:number/reviews`. Flags: `--limit`, `--cursor`, `--target`.

#### `parley pr create <workspace-slug>`
Calls `POST /workspaces/:slug/pulls`. Flags: `--title`, `--head`, `--base` (all required), `--body` or `--body-file`, `--draft`, `--target`.

#### `parley pr comment <workspace-slug> <number>`
Calls `POST /workspaces/:slug/pulls/:number/comments`. Flags: `--body` or `--body-file` (one is required), `--target`.

### 12.7 Background behaviour

- The maintenance loop (§3) deletes deliveries older than 7 days, once at start and then every hour. A failure is logged and retried at the next pass.
- Nothing else in this section runs in the background. Forge reads and writes are synchronous within the API request; nothing is cached or queued, and the hub does not poll the forge.

### 12.8 Events

All events carry the common fields of §14. No forge event ever carries a title of a write, a body, a comment text, a credential, a webhook secret or a forge error message.

**Write events.** The actor is the caller. `workspace` is the workspace whose forge was written; for a call through a task fork that is the parent, and `metadata.fork` holds the fork's slug. Common metadata: `target`, `ok`, `error_type` (when `ok` is false), `number` (when known) and `url` (the web URL of the resulting issue, pull request or comment, when known).

| Event | `resource_type` | `resource_id` | `action` | Extra metadata |
|---|---|---|---|---|
| `parley.forge.issue.create` | `issue` | the new number; `none` on failure | `create` | none |
| `parley.forge.issue.update` | `issue` | number | `update` | `fields` (names of the fields sent, sorted) |
| `parley.forge.issue.close` | `issue` | number | `close` | `comment_url` (when a comment was posted) |
| `parley.forge.comment.create` | `comment` | issue number | `create` | none |
| `parley.forge.label.add` | `label` | issue number | `add` | `labels` |
| `parley.forge.label.remove` | `label` | issue number | `remove` | `label` |
| `parley.forge.label.create` | `label` | label name | `create` | `label`, `created` (true only when this call created it) |
| `parley.forge.pull_request.create` | `pull_request` | the new number; `none` on failure | `create` | `head`, `base`, `draft` |
| `parley.forge.pull_request.comment` | `pull_request` | number | `comment` | none |

**Webhook events.** The actor is the caller; `resource_type` is `webhook` and `resource_id` is the slug.

| Event | `action` | Metadata |
|---|---|---|
| `parley.forge.webhook.set` | `set` | `generated` (boolean) |
| `parley.forge.webhook.unset` | `unset` | `existed` (boolean) |

**Delivery events.** `actor_type` is `forge` and `actor_id` is null. `action` is the last segment of the type. Common metadata: `forge`, `target`, `delivery_id` and `actor` (the forge login that caused the delivery, `""` when the payload names none).

| Event | `resource_type` | `resource_id` | Extra metadata |
|---|---|---|---|
| `parley.forge.issue.opened`, `.edited`, `.closed`, `.reopened` | `issue` | number | `number`, `title` (cut to 256 bytes), `url`, `labels` (at most 100 names) |
| `parley.forge.issue.labeled`, `.unlabeled` | `issue` | number | `number`, `label`, `labels` (the current set, at most 100), `url` |
| `parley.forge.comment.created` | `comment` | issue or pull request number | `number`, `comment_url`, `pull_request` (boolean) |
| `parley.forge.pull_request.opened`, `.closed`, `.merged`, `.reopened`, `.synchronize` | `pull_request` | number | `number`, `url`, `head`, `base`, `merged` (boolean) |
| `parley.forge.push` | `ref` | the ref | `ref`, `before`, `after` |

A merged pull request yields `.merged` and never also `.closed`. `parley.forge.push` reports a push that the forge received. It is unrelated to `parley.git.push` (§5), which reports a push that the hub received.

### 12.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `server.external_url` | string | `""` | base of the webhook URL. When empty, the URL is reported as `null`; a secret can still be set |
| workspace setting `forge_api_url` | URL | `""` | API URL of the origin's forge; also decides the forge kind. `""` means the default for the host |
| workspace setting `upstream_forge_api_url` | URL | `""` | the same for the upstream |
| workspace secret `FORGE_WEBHOOK_SECRET` | string | unset | the webhook secret |
| `HTTPS_PROXY`, `HTTP_PROXY`, `NO_PROXY` (environment) | | unset | proxying of forge calls |

The hub reads no other environment variable for the forge. In particular it ignores `GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN`, `GITHUB_API_URL` and `GITLAB_API_URL`.

The limits stated in §12.4 (deadlines, redirects, sizes, lengths, retention) are fixed and not configurable.

### 12.10 Edge cases and error handling

- A token without the forge scope gets `403` even for a slug that does not exist, because the scope check comes first (§3). A read-only member's write is `403` even on an archived workspace, because access comes before state.
- Malformed JSON is `400` even when the forge cannot be detected. An oversized body on a workspace with an unknown forge is `422`, because the limit depends on the forge kind.
- Ingress with both scheme headers is treated as GitHub; a wrong GitLab token is then irrelevant. A scheme header given twice is `401`.
- An oversized GitHub delivery answers `413` whatever its signature, because the signature covers the body and the body is not read past the limit.
- A correctly signed GitHub delivery in the legacy form encoding is recorded as `malformed` and answered `400`. A delivery with only the legacy SHA-1 signature header is `401`.
- A redelivery of a `malformed` delivery is answered `202` with `duplicate`.
- A `FORGE_WEBHOOK_SECRET` stored through the secrets API (§11) is not bound by the 16-to-255 rule; any non-empty value works at the ingress.
- A task fork never receives deliveries. Events for the parent's repository arrive on the parent's slug.
