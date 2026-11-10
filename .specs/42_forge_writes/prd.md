---
spec_id: "42"
spec_name: "forge_writes"
title: "Write issues and pull requests through the hub"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "e04754e760ded9b7281cc95df69571deacb5273b7fd8dc8cb6746662fcbfb7da"
schema_version: 3
source: "docs/prd/12_forge.md"
---

# Write issues and pull requests through the hub

## Intent

A caller with write access to a workspace creates and changes the issues, comments, labels and pull requests of that workspace's forge repository through the hub, with the credential the hub stores for the workspace, so that no agent ever holds a forge token. The hub never writes to a forge without a stored credential, refuses a bad request before anything is sent, and leaves exactly one hub event for every write that reached the forge, whether it succeeded or failed.

## Goals

- Nine write routes under `/api/v1/workspaces/:slug` work on GitHub and on GitLab: create issue, update issue, close issue, comment on an issue, add labels, remove a label, ensure a label, open a pull request, comment on a pull request.
- Every write route needs the scope `forge:write` and write access, is refused with `409 workspace_archived` on an archived workspace and with `422 forge_credential_missing` when the workspace has no stored credential for the target; a refused request sends nothing to the forge and records no event.
- Text is measured before anything is sent: more than 65,536 characters on GitHub or 1,000,000 on GitLab is `413 payload_too_large`.
- A write that sent at least one request to the forge records exactly one `parley.forge.*` event before its response, with `ok` true or false, and never a title, a body, a comment text, a credential or a forge error message.
- The five `POST` routes that create something can be retried with an `Idempotency-Key`: the replay sends nothing to the forge and records no event.
- A write runs to its end and records its event when the caller disconnects.
- Nine CLI commands call the routes: `parley issue create|update|close|comment|label|unlabel`, `parley label create`, `parley pr create|comment`.

## Non-goals

- Forge detection, the remote target, the forge client and its outbound rules (user agent, proxy, redirects, the 30-second deadline, the 8 MiB read limit, the rate-limit retry), the `:number` rule, the resolution of a task fork to its parent's forge, the error mapping and the scrubbing of forge messages (FRG-FR-001 to FRG-FR-013, FRG-FR-029 to FRG-FR-031), the read routes and the read commands of `parley forge|issue|pr`: package 41 `forge_reads`. This package states what a caller of a write route observes of those rules and builds on that package's code.
- The webhook secret routes `PUT` and `DELETE …/forge/webhook`, their events `parley.forge.webhook.set` and `.unset`, the ingress `/hooks/forge/:slug`, the delivery log and `parley forge webhook|deliveries` (FRG-FR-032 to FRG-FR-041): package 43 `forge_webhook_ingress`.
- The error envelope, the order of the platform's checks, the rules of `Idempotency-Key` and the body size and content type rules (PLAT-FR-010 to PLAT-FR-019): package 02 `http_api_conventions`.
- The CLI's settings, HTTP client, output and exit codes (PLAT-FR-090 to PLAT-FR-095): package 09 `cli_foundation`.
- The event record, its common fields, visibility and the event stream: package 52 `durable_event_log_and_stream`.
- The workspace settings `forge_api_url` and `upstream_forge_api_url`: package 11 `workspace_read_update_settings`. The git credentials of a workspace: packages 10 `workspace_create_and_clone` and 39 `secrets`; the upstream credential for the forge is resolved by package 41 with the secret names and the order of package 31 `carry_patch_setup_and_patches`.
- Pushing a branch to origin so that a pull request can name it: package 13 `workspace_push_to_origin`.
- Updating, merging or closing a pull request, deleting a comment or a label, resolving GitLab assignee names to user ids: the source PRD has no such route (§12.5).
- Any link between a forge object and a change: the two are independent (§12.1).

## Background

The repository holds no code yet; every file named here is new, except the files other packages create and this one extends: `internal/cli/issue.go` and `internal/cli/pr.go`, which package 41 creates for its read commands.

Package 41 delivers, in `internal/forge/`: the resolution of a route's workspace, target and forge configuration (repository, forge kind, API URL, credential kind, and for a task fork the parent); the forge client with its outbound rules; the result types issue, comment and pull request and their mapping from GitHub and GitLab answers (package 41 has no label type; this package adds one, see Design Decision 7); the fake forge `internal/forge/forgetest/`, which this package extends with the write endpoints; the mapping of a failed forge call to a status and an `error_type`; and the scrubbing of forge messages. This package adds the write routes on top of them, in new files of `internal/forge/`, and the write commands in `internal/cli/`. Where this document says "package 41's …", the coder uses whatever name that package gave the part and does not write a second one.

Hub events are recorded through the event component of package 52 in `internal/event/`. `Idempotency-Key` is handled by the create-route helper of package 02 in `internal/server/`. The CLI commands are declared with the command helper of package 09 in `internal/cli/`.

## Requirements

### Access and order of checks on a write route (FRG-FR-001, FRG-FR-002, FRG-FR-004, FRG-FR-007, FRG-FR-025, §12.10)

The nine write routes are:

| Route | Does | Success | Event |
|---|---|---|---|
| `POST /workspaces/:slug/issues` | create issue | `201`, the issue | `parley.forge.issue.create` |
| `PATCH /workspaces/:slug/issues/:number` | update issue | `200`, the issue | `parley.forge.issue.update` |
| `POST /workspaces/:slug/issues/:number/close` | close issue | `200`, the issue | `parley.forge.issue.close` |
| `POST /workspaces/:slug/issues/:number/comments` | comment on an issue | `201`, `{"url"}` | `parley.forge.comment.create` |
| `POST /workspaces/:slug/issues/:number/labels` | add labels | `204` | `parley.forge.label.add` |
| `DELETE /workspaces/:slug/issues/:number/labels/:name` | remove a label | `204` | `parley.forge.label.remove` |
| `POST /workspaces/:slug/labels` | ensure a label | `200` or `201`, the label | `parley.forge.label.create` |
| `POST /workspaces/:slug/pulls` | open a pull request | `201`, the pull request | `parley.forge.pull_request.create` |
| `POST /workspaces/:slug/pulls/:number/comments` | comment on a pull request | `201`, `{"url"}` | `parley.forge.pull_request.comment` |

Every write route needs the scope `forge:write` and write access to the workspace, and takes the query parameter `target`. A PAT without the scope gets `403 insufficient_scope`, also for a slug that does not exist. A caller with read access only (a read-only org member) gets `403 forbidden`, also on an archived workspace, because access comes before state. A caller with no access gets `404 not_found`.

After the platform's checks, a write route checks in this order, and the first failure decides the answer (FRG-FR-002):

1. `target`: given twice or with a value other than `origin` and `upstream` → `400 invalid_request`; `upstream` on a workspace that is not a carry-patch workspace with an `upstream_url` → `400 workspace_mode_mismatch`.
2. The path parameters and the body, as far as they do not depend on the forge kind → `400 invalid_request`. A bad `:number` has the message `number must be a positive decimal integer`.
3. The workspace is archived → `409 workspace_archived`; the route was called through a task fork whose parent is archived → `409 parent_archived`.
4. Forge configuration → `422 forge_api_url_invalid` or `422 forge_unknown`.
5. Checks that depend on the forge kind: GitLab assignees (FRG-FR-017) → `400`; text limits (FRG-FR-024) → `413`.
6. The credential kind for the target is `none` → `422 forge_credential_missing` (FRG-FR-025).
7. The call.

A request refused before step 7 sends nothing to the forge, including the lookups some writes begin with, and records no event. Two consequences the source PRD names: malformed JSON is `400` even when the forge cannot be detected, and an oversized text on a workspace with an unknown forge is `422`, because the limit depends on the forge kind.

No write route needs the hub's copy of the repository, and none answers `workspace_busy`.

**Task forks.** A write route called with a fork's slug acts on the parent's forge repository with the parent's credential and the parent's settings, including `target=upstream`. The caller must have write access on the parent; without it the answer is `403 forbidden`. A token bound to the fork is accepted; the access that counts is that of the user behind it.

**Upstream.** With `target=upstream` on a carry-patch workspace that has an `upstream_url`, the write goes to the upstream repository, through the API URL of `upstream_forge_api_url` or the default of the upstream host, with the upstream git credential, which falls back to the origin credential.

### Bodies and text limits (FRG-FR-014, FRG-FR-024)

A write body is a JSON object, except on the add-labels route, where it is a bare JSON array, and on the close route, where it may also be empty. An absent field and a `null` field both mean "not set". Unknown fields are ignored. A required string must not be blank. A list of names has at most 100 entries, none blank. A violation is `400 invalid_request`, and the message names the field.

Long text is refused before anything is sent: more than 65,536 characters on GitHub, more than 1,000,000 on GitLab, counted in Unicode code points. The measured fields are the `body` of an issue (create and update), of a comment and of a pull request, and the `comment` of a close. Over the limit → `413 payload_too_large`, with a message that names the field, its length and the limit. A text of exactly the limit is accepted.

### Create and update an issue (FRG-FR-015 to FRG-FR-017)

**Create.** Body `{"title": string, "body"?: string, "labels"?: [string], "assignees"?: [string]}`. `title` is required. Answers `201` with the issue.

**Update.** Body with any of `title` (not blank), `body` (`""` clears it), `state` (`open` or `closed`), `labels` and `assignees` (each replaces the whole set; `[]` clears it). Only the fields that are set are sent. No field set → `400 invalid_request`. Answers `200` with the issue as the forge returns it.

**GitLab assignees.** GitLab takes assignees as user ids, which the hub does not resolve. On GitLab `assignees` is not sent on create or update; the other fields are. An update whose only set field is `assignees` is refused with `400 invalid_request`; nothing is sent.

### Close an issue (FRG-FR-018)

The body may be empty or `{"comment": string}`. A non-blank comment is posted first; the issue is closed only if the comment succeeded. If the comment fails, the answer is that failure and the issue is untouched. If the close fails after the comment was posted, the answer is the close's failure and the event carries `comment_url`. Closing a closed issue succeeds. Answers `200` with the issue.

### Comment on an issue or a pull request (FRG-FR-019)

Body `{"body": string}`, required. Answers `201` with `{"url": "<web URL of the comment>"}`. On the pull-request route the comment goes to the pull request's conversation.

### Labels (FRG-FR-020 to FRG-FR-022)

**Add labels.** The body is a bare JSON array of 1 to 100 non-blank names. The labels are added and the issue keeps its others. Answers `204`.

**Remove a label.** The label is the last path segment, percent-decoded once, so `type%2Fbug` names `type/bug`. A blank or undecodable name is `400`. The route is idempotent: `204` when the issue exists, whether or not it carried the label; `404 forge_not_found` when the issue does not exist.

**Ensure a label.** Body `{"name": string, "color"?: string, "description"?: string}`. `name` is required; `color` is six hex digits with or without a leading `#`, default `ededed`. If the repository has a label of that name, the answer is `200` with the label as the forge holds it, unchanged. Otherwise the label is created and the answer is `201`. If two callers race, the loser answers `200` with the winner's label.

### Open a pull request (FRG-FR-023)

Body `{"title": string, "head": string, "base": string, "body"?: string, "draft"?: boolean}`. `title`, `head` and `base` are required; `draft` defaults to `false`. `head` is sent as given, so `owner:branch` names a branch of a forge-side fork. Both branches must exist on the forge. A pull request that already exists for the same head and base is the forge's refusal (`422 forge_rejected`). Answers `201` with the pull request.

### Forge failures, disconnects and replays (FRG-FR-026, FRG-FR-027, FRG-FR-029 to FRG-FR-031)

A forge call of a write that fails is answered with the mapping of package 41: `404 forge_not_found`, `422 forge_rejected`, `429 forge_rate_limited` (with `Retry-After` when the forge stated a wait), `502 forge_unauthenticated`, `502 forge_forbidden`, `502 forge_error`, `502 forge_unreachable`, `504 forge_timeout`. The message is scrubbed and carries no credential.

A write is not abandoned when the caller disconnects. It runs to its end and its event is recorded.

The five `POST` routes that create something (issue, issue comment, pull-request comment, label, pull request) accept `Idempotency-Key` as §3 specifies: a replay answers `200` with the original body, sends nothing to the forge and records no event.

### Events (FRG-FR-028, §12.8)

A write route records exactly one `parley.forge.*` event when it sent at least one request to the forge, whether the write succeeded or failed. The event is recorded before the response is sent.

The actor is the caller. `workspace` is the workspace whose forge was written; for a call through a task fork that is the parent, and `metadata.fork` holds the fork's slug. Common metadata: `target`, `ok`, `error_type` (when `ok` is false), `number` (when known) and `url` (the web URL of the resulting issue, pull request or comment, when known).

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

No forge event ever carries a title of a write, a body, a comment text, a credential or a forge error message.

### CLI (§12.6)

The conventions of §3 apply. Every command prints the hub's answer unless a different output is stated. Slugs, numbers and label names are sent as single percent-encoded path segments. A flag that is not given sends nothing. `<number>` is checked with the rule of FRG-FR-006 before anything is sent; a violation is a usage error (exit 2). Every command takes `--target <origin|upstream>`. Text input: `--body <text>` gives the text; `--body-file <path>` reads it from a file, or from stdin when the path is `-`. The text is sent exactly as given. Giving both is a usage error.

| Command | Route | Flags and output |
|---|---|---|
| `parley issue create <workspace-slug>` | `POST …/issues` | `--title` (required), `--body` or `--body-file`, `--label` (repeatable, one name each), `--assignee` (repeatable) |
| `parley issue update <workspace-slug> <number>` | `PATCH …/issues/:number` | `--title`, `--body` or `--body-file`, `--state`, `--label` (repeatable), `--assignee` (repeatable); only the given fields are sent; no field flag is a usage error; `--body ""` clears the body |
| `parley issue close <workspace-slug> <number>` | `POST …/issues/:number/close` | `--comment <text>` |
| `parley issue comment <workspace-slug> <number>` | `POST …/issues/:number/comments` | `--body` or `--body-file` (one is required) |
| `parley issue label <workspace-slug> <number> <label>...` | `POST …/issues/:number/labels` | prints `{"status":"labeled","number":<n>,"labels":[…]}` |
| `parley issue unlabel <workspace-slug> <number> <label>` | `DELETE …/issues/:number/labels/:name` | prints `{"status":"unlabeled","number":<n>,"label":"<label>"}` |
| `parley label create <workspace-slug> <name>` | `POST …/labels` | `--color <hex>`, `--description <text>`; exit 0 whether the label existed or was created |
| `parley pr create <workspace-slug>` | `POST …/pulls` | `--title`, `--head`, `--base` (all required), `--body` or `--body-file`, `--draft` |
| `parley pr comment <workspace-slug> <number>` | `POST …/pulls/:number/comments` | `--body` or `--body-file` (one is required) |

### End-to-end flows

- An agent runs `parley issue create demo --title … --label bug`, then `parley issue label demo 7 triage` and `parley issue comment demo 7 --body …`. The parley CLI sends each request; `parleyd` checks scope, access, the body, the workspace state, the forge configuration, the text limit and the credential, calls the forge with the workspace's stored token, records one event per write and answers. The forge holds the issue, both labels and the comment; the event log holds three events with `ok` true; no answer and no event contains the token.
- An agent sends `POST /workspaces/demo/pulls` with an `Idempotency-Key`, loses the answer and sends the same request again. `parleyd` opens the pull request on the forge once, records one `parley.forge.pull_request.create` event, and answers the second request `200` with the first answer without contacting the forge.

## Design Decisions

1. **Language and tests (project-wide).** Go; HTTP uses `net/http` and its method-and-pattern router; the CLI uses `github.com/spf13/cobra`; tests are table-driven with `testing` and `net/http/httptest`. A test forge is an `httptest` TLS server that plays GitHub or GitLab; the handlers are built with the forge client's transport as a parameter so that tests trust it. In tests the forge kind comes from the remote's host (`github.com`, `gitlab.com`) or from a `forge_api_url` whose path contains `github` or `gitlab` (the kind rule of §12.3 looks at the whole URL).
2. **Webhook routes are not in this package.** FRG-FR-001 and FRG-FR-004 call the webhook `PUT` a write, but the plan gives webhook secret management to package 43. "Write route" here means the nine routes above. Package 43 applies the same preconditions to its `PUT`, in the same order where both have them: scope `forge:write`, write access (`403 forbidden` for read access only, also on an archived workspace), the body, then `409 workspace_archived`. Its routes take no `target` and refuse a task fork with `400 fork_has_no_origin` instead of acting on the parent, so they never answer `parent_archived`; they call no forge, so the forge configuration and credential steps do not apply; `503 secrets_unavailable` and `429 secret_limit_reached` are theirs alone, because no route of this package stores a secret.
3. **Blank** means empty or made of Unicode white space only. The source PRD does not define it.
4. **What the hub sends** (unverified; taken from the forges' public REST APIs, to be confirmed by the coder against the forges' documentation). Paths are relative to the API URL; `R` is `/repos/<owner>/<name>` on GitHub and `/projects/<owner/name as one percent-encoded segment>` on GitLab; label names in a path are one percent-encoded segment.

   | Write | GitHub | GitLab |
   |---|---|---|
   | create issue | `POST R/issues` `{title, body, labels, assignees}` | `POST R/issues` `{title, description, labels}` |
   | update issue | `PATCH R/issues/<n>` `{title, body, state, labels, assignees}` | `PUT R/issues/<n>` `{title, description, state_event, labels}`; `state` `closed` is `state_event` `close`, `open` is `reopen` |
   | close issue | `PATCH R/issues/<n>` `{state: "closed"}` | `PUT R/issues/<n>` `{state_event: "close"}` |
   | comment on an issue | `POST R/issues/<n>/comments` `{body}` | `POST R/issues/<n>/notes` `{body}` |
   | comment on a pull request | `POST R/issues/<n>/comments` `{body}` | `POST R/merge_requests/<n>/notes` `{body}` |
   | add labels | `POST R/issues/<n>/labels` `{labels}` | `PUT R/issues/<n>` `{add_labels}` |
   | remove a label | `DELETE R/issues/<n>/labels/<name>`; on `404`, `GET R/issues/<n>` | `PUT R/issues/<n>` `{remove_labels}` |
   | ensure a label | `GET R/labels/<name>`, then `POST R/labels` `{name, color, description}` | `GET R/labels/<name>`, then `POST R/labels` `{name, color: "#<hex>", description}` |
   | open a pull request | `POST R/pulls` `{title, head, base, body, draft}` | `POST R/merge_requests` `{title, source_branch, target_branch, description}` |

   A field that is not set is left out of the forge request. On GitLab a list of label names is sent as one comma-separated string, as its API takes it; the hub does not refuse a name that contains a comma.
5. **Remove a label on GitHub.** GitHub answers `404` both when the issue is missing and when the issue does not carry the label. On a `404` the hub reads the issue: if it exists the answer is `204`, otherwise `404 forge_not_found`. Any other failure of either call is mapped as usual.
6. **Ensure a label.** The hub looks the label up by name first. A `404` of the lookup means "no such label" and leads to the create; any other lookup failure is the answer. When the create is refused because the label now exists (GitHub `422`, GitLab `409`), the hub looks the label up once more and answers `200` with it; if that lookup does not find it, the answer is the create's mapped refusal. An existing label is never changed, also when the request's `color` or `description` differ.
7. **Colour form.** `color` is accepted in either letter case and sent in lower case, without `#` to GitHub and with `#` to GitLab. Package 41 reads no labels as objects, so this package adds the label type `{name, color, description}` to `internal/forge/types.go` with its GitHub and GitLab mappings; `color` in an answer is six lower-case hex digits without `#`, and an absent description is `""`.
8. **A draft on GitLab** is a merge request whose title begins with `Draft: `; the hub adds that prefix when `draft` is true and the title does not already begin with it. `head` is sent as `source_branch` as given; the hub does not resolve `owner:branch` to a GitLab project.
9. **The comment's web URL** comes from package 41's comment mapping, the one its comments list uses, so that a comment has one URL whether it is read or written.
10. **`fields` of the update event** names the fields sent to the forge, sorted. On GitLab `assignees` is never among them.
11. **Which `POST` routes take `Idempotency-Key`.** The five that create something. The close route and the add-labels route create no resource; the header is ignored there and each request is carried out.
12. **A request counts as sent** from the moment the hub hands it to the forge client, also when the connection fails or the deadline passes. Such a write records its event with `ok` false.
13. **The write survives a disconnect** because the forge calls and the event write do not use the request's cancellation; each forge request keeps its own 30-second deadline.
14. **An event that cannot be recorded** makes the answer `500 internal`. What the forge did is not undone. The source PRD does not say; a success without its event would break "exactly one event". Events are recorded with the event component of package 52 (`EmitNow`, since a forge write has no store transaction of its own). Package 43 answers `500 internal` in the same case on its management routes, where the secret change and the event share one transaction and are both rolled back.
15. **`url` and `number` in the event.** `number` is the issue or pull-request number of the path, or the new number after a create; it is absent on `parley.forge.label.create` and on a failed create. `url` is the web URL of the issue (create, update, close), of the pull request (create) or of the comment (the two comment routes); it is absent on the three label events and on a failure that produced no object.
16. **The close body.** Empty, `{}`, `{"comment": null}` and a blank `comment` all close without a comment. A `comment` that is not a string is `400 invalid_request`.
17. **An undecodable label segment** (`%zz`) never reaches the handler: the HTTP layer of package 02 answers `400` for a request target that cannot be parsed. `%252F` decodes once, to the name `%2F`.
18. **CLI details the source PRD leaves open.** A missing required flag, both `--body` and `--body-file`, a `--body-file` that cannot be read and `issue update` without a field flag are `usage_error` (exit 2), raised before any request. `parley issue close` sends no body without `--comment` and `{"comment": <text>}` with it. `--state` is sent as given; the hub validates it. `--draft` not given sends no `draft` member. `--target` and the `<number>` check are the helpers of package 41's read commands. The CLI sends no `Idempotency-Key`.
19. **Nothing is stored per workspace** by this package, so it registers nothing with the workspace deletion registry of package 14.

## Dependencies

- **41 forge_reads** (draft): forge detection and configuration, the forge client and its outbound rules, the result types, the error mapping and scrubbing, the fork-to-parent resolution, and the `parley issue` and `parley pr` command groups this package extends.
- **09 cli_foundation** (draft): the command helper, HTTP client, output and exit codes the nine commands are built on.
- **52 durable_event_log_and_stream** (draft): the event component the write events are recorded through.
- Through them: 02 http_api_conventions (error envelope, check order, `Idempotency-Key`), 07 workspace_access_rule, 08 workspace_bound_tokens (a fork-bound token passes the guard on a forge route of its fork), 10 workspace_create_and_clone and 39 secrets (the stored git credential), 11 workspace_read_update_settings (`forge_api_url`), 03 server_lifecycle_and_configuration (the scrubber in `internal/scrub/`).
- External: the REST APIs of GitHub and GitLab (Design Decisions, 4).

## Verified External API

- `github.com/spf13/cobra` — **unverified** (not yet in `go.mod`): `cobra.Command` with `Use`, `Short`, `Args`, `RunE func(cmd *cobra.Command, args []string) error`; `cobra.ExactArgs(n int) cobra.PositionalArgs`; `cobra.MinimumNArgs(n int) cobra.PositionalArgs`; `(*Command).AddCommand(cmds ...*Command)`; `(*Command).Flags() *pflag.FlagSet`.
- The GitHub and GitLab REST endpoints of Design Decisions, 4 — **unverified**: written from the forges' public API as generally documented, not read in this session.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Forge**: GitHub or GitLab, where a workspace's repository lives.
- **Forge kind**: `github` or `gitlab`, decided from the API URL setting or the remote's host (§12.3).
- **Remote target**: The repository a forge route works on, chosen by the query parameter `target`: `origin` (the default) or `upstream`.
- **Credential kind**: What the hub sends to the forge for a target: `pat` (the stored token), `basic` (the stored password, as the token) or `none`.
- **Write route**: One of the nine routes of this package that change something on the forge.
- **Pull request**: A forge pull request; on GitLab a merge request, numbered by its project-level `iid`.
