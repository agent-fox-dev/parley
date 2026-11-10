---
spec_id: "41"
spec_name: "forge_reads"
title: "Read issues and pull requests through the hub"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "07261d8a129da8c5df37f4b591346637ff9d5c493e858c3af3aa7b3d07928e6d"
schema_version: 3
source: "docs/prd/12_forge.md"
---

# Read issues and pull requests through the hub

## Intent

A workspace's repository usually lives on GitHub or GitLab, and the hub is that repository's forge client: a caller reads the repository's issues, comments and pull requests through the hub, with the credential the hub stores for the workspace, and never holds a forge token. The hub works out by itself which forge a workspace uses and how to reach it, answers both forges in one shape, turns every forge failure into one documented error, and never lets a forge credential leave it.

## Goals

- For every workspace the hub derives the forge repository (`host`, `owner`, `name`), the forge kind (`github`, `gitlab`), the API URL and the credential kind (`pat`, `basic`, `none`) from the workspace's remote URL, two settings and its stored git credential, for the origin and, on a carry-patch workspace, for the upstream.
- `GET /workspaces/:slug/forge` reports that derivation without contacting the forge.
- Nine read routes answer the repository, issues, issue threads, comments, pull requests, pull request state, changed files, check runs and reviews in one shape for GitHub and GitLab.
- A read with no stored credential is sent anonymously; no forge token is ever read from the hub's environment.
- Every outbound request carries `User-Agent: parleyd/<version>`, has a 30-second deadline, reads at most 8 MiB, follows a redirect only to the same scheme, host and port (at most 10), and is retried once after a rate limit when the stated wait fits the deadline.
- Every forge failure maps to one status and `error_type` by a fixed table; every message is scrubbed of the workspace's token and cut to 1,024 bytes.
- A read route called with a task fork's slug acts on the parent's forge repository with the parent's credential and settings.
- Ten CLI commands (`parley forge status|repository`, `parley issue list|get|comments`, `parley pr get|state|files|checks|reviews`) call these routes.
- Packages 42 and 43 build on the same forge component: resolution, the outbound client, the error mapping and the shared route checks.

## Non-goals

- Write routes (create and update an issue, close, comment, labels, pull request create and comment), the missing-credential refusal `forge_credential_missing`, body length limits, the archived and `parent_archived` refusals of writes, idempotency of writes, the `parley.forge.*` write events and the write commands of `parley issue|label|pr` (FRG-FR-014 to FRG-FR-028): package 42 forge_writes. This package delivers the client and the shared route checks those routes use.
- The webhook secret routes (`PUT` and `DELETE …/forge/webhook`), the ingress `POST /hooks/forge/:slug`, the delivery log and its pruning, `GET …/forge/deliveries`, the refusal `fork_has_no_origin` on those routes, and the commands `parley forge webhook set|unset` and `parley forge deliveries` (FRG-FR-032 to FRG-FR-041): package 43 forge_webhook_ingress. This package reports `webhook.configured` and `webhook.url` in the diagnostic document and takes `webhook.last_delivery_at` from a provider package 43 registers.
- Definition, defaults and stored validation of the workspace settings `forge_api_url` and `upstream_forge_api_url`: package 11 workspace_read_update_settings. This package reads them and checks them again at use.
- Storing, encrypting and rotating the secrets a credential is made of: package 39 secrets. Storing the origin credential at workspace creation and the origin credential resolution: package 10 workspace_create_and_clone.
- Creating a carry-patch workspace and its upstream set-up: package 31 carry_patch_setup_and_patches. This package reads `workspace_mode` and `upstream_url` from the workspace record package 10 stores.
- Creating task forks: package 15 task_fork_creation. The binding guard that lets a fork-bound token reach the fork's forge routes: package 08 workspace_bound_tokens.
- The error envelope, check order, list shape, request limits and scope check: packages 02, 04, 06 and 07. The CLI's client, output and exit codes: package 09 cli_foundation.
- Pushing branches to origin (package 13), registering a webhook on the forge (the hub never does it), forges other than GitHub and GitLab, caching or polling the forge.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/12_forge.md` (§12), requirements FRG-FR-001 to FRG-FR-013 and FRG-FR-029 to FRG-FR-031, with the parts of §12.3, §12.5, §12.6, §12.9 and §12.10 that belong to them.

This package creates the forge component in `internal/forge/`:

| File | Holds |
|---|---|
| `resolve.go` | remote URL to repository, forge kind, API URL, credential kind |
| `client.go` | the outbound HTTP client: headers, redirects, deadline, size limit, rate-limit retry |
| `errors.go` | the failure mapping, messages and scrubbing |
| `route.go` | the checks shared by every forge route: `target`, `:number`, task fork to parent, configuration |
| `page.go` | forge list paging and cursors |
| `types.go` | the result types |
| `github.go`, `gitlab.go` | the two wire mappings |
| `status.go`, `issues.go`, `pulls.go` | the route handlers |
| `service.go` | the component's constructor and its registration points |
| `forgetest/` | a fake forge (GitHub and GitLab) served by `net/http/httptest`, for this package's tests and those of packages 42 and 43 |

Tests sit beside each file (`resolve_test.go`, …). The CLI commands are in `internal/cli/forge.go`, `internal/cli/issue.go` and `internal/cli/pr.go`, one file for each command group: this package creates the three files and their groups, package 42 adds its write commands to `issue.go` and `pr.go` (and creates `label.go`), and package 43 adds `webhook` and `deliveries` to `forge.go`; the end-to-end test is `test/e2e/forge_read_test.go`. The component is composed into the process in `internal/hub/hub.go` (package 03).

It uses: the route registration, error envelope and list helper of `internal/server/` (package 02); the scope check (package 06) and the access component `internal/access/` (package 07); the workspace record, the settings reader and the origin credential resolution of `internal/workspace/` (packages 10 and 11); workspace secrets from `internal/secret/` (package 39); the scrubber `internal/scrub/` (package 03); the build version of `internal/buildinfo/` (package 01); the CLI foundation `internal/cli/` (package 09).

## Requirements

### Remote target (§12.3, FRG-FR-003)

The query parameter `target` selects the repository a route works on. Absent or `origin`: the workspace's `git_url`, the origin git credential and the setting `forge_api_url`. `upstream`: the workspace's `upstream_url`, the upstream git credential and the setting `upstream_forge_api_url` (no fallback to `forge_api_url`); only for a carry-patch workspace that has an `upstream_url`.

`target` given more than once, or with any other value, is `400 invalid_request`. `target=upstream` on a workspace that is not a carry-patch workspace with an `upstream_url` is `400` with `error_type` `workspace_mode_mismatch`. The diagnostic route takes no `target`; one that is given is ignored.

### Repository, forge kind, API URL (§12.3, FRG-FR-005)

**Repository.** The hub derives `host`, `owner` and `name` from the remote URL. It accepts `https://`, `http://`, `ssh://` and `git://` URLs and the scp form `user@host:owner/name`. The host is lower-cased, a leading `www.` and any port are dropped, userinfo is ignored and a trailing `.git` is removed. The path needs at least two segments: `name` is the last, `owner` is everything before it (`group/subgroup` is kept). A URL that gives fewer names no repository.

**Forge kind**, in this order: (1) the API URL setting is set and contains exactly one of `github` and `gitlab`: that forge; (2) otherwise the remote's host contains exactly one of the two: that forge; (3) otherwise unknown.

**API URL.** The setting, when set, with one trailing `/` removed. Otherwise the default for the host: `https://api.github.com` for `github.com`, `https://<host>/api/v3` for another GitHub host, `https://<host>/api/v4` for GitLab. A GitLab API URL that does not end in `/api/v4` gets it appended.

**Resolution failures** on a route that calls the forge, first failure wins: the setting is not an absolute `https` URL with a host and without userinfo, query or fragment → `422` `forge_api_url_invalid`; the remote URL names no repository → `422` `forge_unknown`; the forge kind is unknown → `422` `forge_unknown` with a message that names the setting to set (`forge_api_url` for origin, `upstream_forge_api_url` for upstream).

### Credential kind (§12.3, FRG-FR-010)

Resolved anew on every request from the workspace's secrets, so a rotated secret applies to the next request:

| Kind | Stored | Sent to the forge |
|---|---|---|
| `pat` | `GIT_PAT`, set and not empty | the token |
| `basic` | `GIT_USERNAME` and `GIT_PASSWORD`, both set and not empty | the password, as the token |
| `none` | neither | no authentication |

For `upstream` the order is `UPSTREAM_GIT_PAT` when set and not empty, then `UPSTREAM_GIT_USERNAME` with `UPSTREAM_GIT_PASSWORD` when both are set and not empty, then the origin resolution. A user name without a password is skipped, and an empty value counts as not set. If the secrets cannot be read the request answers `500 internal` and nothing is sent; it never falls back to anonymous. The hub never reads `GITHUB_TOKEN`, `GH_TOKEN`, `GITLAB_TOKEN`, `GITHUB_API_URL` or `GITLAB_API_URL` (§12.9).

### Access and order of checks (FRG-FR-001, FRG-FR-002, FRG-FR-004, FRG-FR-006)

Every read route needs the scope `forge:read` (a token with `forge:write` passes) and read access to the workspace, so a read-only org member reads. A token without the scope gets `403 insufficient_scope` whether or not the slug exists; a caller without access gets the `404 not_found` of an unknown slug.

After access the steps run in this order, and the first failure answers: (1) `target`; (2) path and query parameters; (3) forge configuration; (4) the call. A request refused before the call sends nothing to the forge. Reads work in every workspace state (`pending`, `ready`, `failed`, archived, busy): they never answer `workspace_archived`, `workspace_not_ready` or `workspace_busy`, and they do not use the hub's copy of the repository. Reads record no events, and nothing is cached: every request calls the forge.

A `:number` path parameter is a positive decimal integer: digits only, no sign, no leading zero, at most 2,147,483,647. Anything else is `400 invalid_request` with the message `number must be a positive decimal integer`.

### Task forks (FRG-FR-007)

A task fork has no forge of its own. A read route (the diagnostic route included) called with a fork's slug acts on the parent's forge repository with the parent's credential and settings, including `target=upstream`. The caller needs read access on the parent; without it the answer is `403 forbidden` and nothing is sent. A token bound to the fork is accepted; the access that counts is that of the user behind it.

### Outbound calls (FRG-FR-008, FRG-FR-009, §12.9)

- Every request carries `User-Agent: parleyd/<version>`, and the credential's token when the kind is `pat` or `basic`.
- `HTTPS_PROXY`, `HTTP_PROXY` and `NO_PROXY` are honoured.
- A redirect is followed only to the same scheme, host and port, at most 10 times.
- Each request has a 30-second deadline. A response body is read up to 8 MiB (8,388,608 bytes); a longer one is something the hub cannot read.
- **Rate limit.** A forge answer is rate limited when its status is 429, or 403 with `X-RateLimit-Remaining: 0` or a `Retry-After` header. The stated wait is `Retry-After` in seconds, else the reset time in `X-RateLimit-Reset` or `RateLimit-Reset` minus now. The request is retried once, after the wait, when the wait is at most the time left of the request's deadline. Otherwise, or when the retry is rate limited too, or when no wait is stated, the caller gets `429`.

### Error mapping (FRG-FR-029 to FRG-FR-031)

First matching row wins:

| Condition | Status | `error_type` |
|---|---|---|
| redirect to another host, port or scheme, or more than 10 redirects | 502 | `forge_error` |
| no answer within the deadline | 504 | `forge_timeout` |
| rate limited, after the retry rule | 429 | `forge_rate_limited` |
| the forge answers 404 | 404 | `forge_not_found` |
| the forge answers 401 | 502 | `forge_unauthenticated` |
| the forge answers 403 (not a rate limit) | 502 | `forge_forbidden` |
| the forge answers any other 4xx | 422 | `forge_rejected` |
| the forge answers 5xx, or something the hub cannot read (over 8 MiB, not the expected JSON) | 502 | `forge_error` |
| the connection fails (DNS, TCP, TLS) | 502 | `forge_unreachable` |

The message is `the forge answered <status>: <the forge's message>`, or a plain description when the forge gave no answer. A `429` carries `Retry-After` in whole seconds when the forge stated a wait. Every message is scrubbed before it leaves the hub: the workspace's token is replaced by `[redacted]`, URL userinfo is removed and the text is cut to 1,024 bytes. No response and no log line carries a forge credential.

### `GET /workspaces/:slug/forge` (§12.5)

Diagnostic; contacts nothing. Answers `200`:

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

`forge` is `github`, `gitlab` or `unknown`. A remote that names no repository, or a forge that cannot be detected, is reported and is not an error: `forge: "unknown"`, `api_url: null`, and `repository: null` when there is none. `upstream` is an object with the same four members (`forge`, `api_url`, `repository`, `credential`) for a carry-patch workspace with an `upstream_url`, else `null`. `webhook.configured` says whether the workspace secret `FORGE_WEBHOOK_SECRET` exists and is not empty; `webhook.url` is `server.external_url` without a trailing `/` followed by `/hooks/forge/<slug>`, or `null` when `external_url` is empty; `webhook.last_delivery_at` is the time of the newest recorded delivery, or `null`. For a task fork the whole document describes the parent, with the parent's slug in the URL. The only forge error is `422 forge_api_url_invalid`, for either setting.

### Read routes (§12.5, FRG-FR-011 to FRG-FR-013)

All take `target`. Timestamps are RFC 3339 UTC with whole seconds; an absent one is `null`. `url` is the web URL of the object.

| Route | Answers |
|---|---|
| `GET /workspaces/:slug/forge/repository` | the repository |
| `GET /workspaces/:slug/issues` | a list of issues; filters below |
| `GET /workspaces/:slug/issues/:number` | the issue thread |
| `GET /workspaces/:slug/issues/:number/comments` | a list of comments, oldest first |
| `GET /workspaces/:slug/pulls/:number` | the pull request |
| `GET /workspaces/:slug/pulls/:number/state` | the pull request state |
| `GET /workspaces/:slug/pulls/:number/files` | a list of changed files |
| `GET /workspaces/:slug/pulls/:number/checks` | a list of the check runs of the pull request's head; empty when it has none |
| `GET /workspaces/:slug/pulls/:number/reviews` | a list of reviews, oldest first |

| Type | Members |
|---|---|
| repository | `full_name`, `default_branch`, `private`, `archived`, `permissions {pull, push, admin}`, `allow_merge_commit`, `allow_squash_merge`, `allow_rebase_merge` (the last three `null` when the forge does not report them) |
| issue | `number`, `title`, `body`, `state` (`open`, `closed`), `url`, `author {login}`, `labels` (names), `created_at`, `updated_at`, `closed_at`, `is_pr` |
| issue thread | `issue`, `comments`, `comments_next_cursor`, `comments_error` |
| comment | `id`, `body`, `author {login}`, `created_at`, `updated_at`, `url` |
| pull request | `number`, `title`, `body`, `state` (`open`, `closed`, `merged`), `url`, `draft`, `merged`, `head_sha`, `head_branch`, `base_branch`, `author {login}`, `created_at`, `updated_at`, `closed_at`, `merged_at` |
| pull request state | `number`, `state`, `merged`, `head_sha` |
| changed file | `filename`, `status` (`added`, `modified`, `deleted`, `renamed`), `additions`, `deletions` |
| check run | `name`, `status` (`queued`, `in_progress`, `completed`), `conclusion` (`success`, `failure`, `cancelled`, `skipped`, `neutral`, or `null` while not completed), `summary`, `url` |
| review | `author {login}`, `state` (`approved`, `changes_requested`, `commented`, `dismissed`), `body`, `submitted_at` |

On GitLab a number is the project-level `iid`; a pull request is a merge request; check runs are the jobs of the merge request's latest pipeline; reviews are the approvals (as `approved`) and the non-system notes (as `commented`). On GitHub the number of a pull request given to the issue route answers the issue with `is_pr: true`.

**Lists** answer `{"items": […], "next_cursor": …}` and take `limit` (1 to 200, default 50) and `cursor`. A cursor is opaque and valid only for the same route with the same parameters; any other use is `400 invalid_request`.

**Issue list filters**, each at most once: `state` (`open`, `closed`, `all`; default `open`), `sort` (`created`, `updated`; default `created`), `direction` (`asc`, `desc`; default `desc`), `labels` (comma-separated names; an issue must carry all of them), `assignee` (a login). A repeated parameter or a value outside the set is `400 invalid_request`. Pull requests are never part of the issue list.

**Issue thread.** The issue plus its first page of comments (at most 100), oldest first. `comments_next_cursor` continues on the comments route and is `null` when the page is complete. If the issue was read but its comments were not, the answer is still `200`, `comments` is `[]` and `comments_error` is `{"error_type", "message"}` with the values the error mapping gives; otherwise `comments_error` is `null`.

### CLI (§12.6)

The conventions of package 09 apply; every command prints the hub's answer. Slugs and numbers are sent as single percent-encoded path segments. A flag that is not given sends nothing.

| Command | Route | Flags |
|---|---|---|
| `parley forge status <slug>` | `GET …/forge` | none |
| `parley forge repository <slug>` | `GET …/forge/repository` | `--target` |
| `parley issue list <slug>` | `GET …/issues` | `--state`, `--label` (repeatable; sent as one `labels` parameter), `--assignee`, `--limit`, `--cursor`, `--target` |
| `parley issue get <slug> <number>` | `GET …/issues/:number` | `--target` |
| `parley issue comments <slug> <number>` | `GET …/issues/:number/comments` | `--limit`, `--cursor`, `--target` |
| `parley pr get <slug> <number>` | `GET …/pulls/:number` | `--target` |
| `parley pr state <slug> <number>` | `GET …/pulls/:number/state` | `--target` |
| `parley pr files <slug> <number>` | `GET …/pulls/:number/files` | `--limit`, `--cursor`, `--target` |
| `parley pr checks <slug> <number>` | `GET …/pulls/:number/checks` | `--limit`, `--cursor`, `--target` |
| `parley pr reviews <slug> <number>` | `GET …/pulls/:number/reviews` | `--limit`, `--cursor`, `--target` |

`<number>` is checked with the `:number` rule before anything is sent; a violation is a usage error (exit 2).

### End-to-end flows

1. **An agent triages from the CLI.** An agent with a `forge:read` token runs `parley issue list widgets --label bug`; the `parley CLI` sends `GET /api/v1/workspaces/widgets/issues?labels=bug`; `parleyd` checks scope and access, resolves the forge from the workspace's `git_url`, `forge_api_url` and stored `GIT_PAT`, and asks the forge with that token; the `forge` answers a page that contains one pull request; `parleyd` answers the issues without it; the CLI prints the list and exits 0. The agent then runs `parley issue get widgets 7` and gets the issue with its comments.
2. **An agent in a task fork reads the parent's forge.** An agent with a `forge:read` token calls `GET /api/v1/workspaces/<fork>/pulls/12/state` with the slug of a task fork; `parleyd` finds the fork's parent, confirms the user's read access on it, calls the parent's forge with the parent's credential and answers the state. When the forge then answers 404 for another number, `parleyd` answers `404 forge_not_found` with a message that does not contain the token.

## Design Decisions

1. **Project-wide.** Go; HTTP with the standard library `net/http`; the CLI with `github.com/spf13/cobra`; tests are table-driven with `testing` and `net/http/httptest`; code takes a replaceable clock.
2. **Dependencies beyond the plan's list.** `09` is added because this package specifies CLI commands. `11` is added because the two forge settings are registered there and the end-to-end flow sets `forge_api_url` through `PATCH /workspaces/:slug`.
3. **The upstream credential is resolved here.** Package 31 is not a dependency (its resolver lives in `internal/carrypatch`), so this package reads the workspace secrets itself through package 39's `Get`, with the names and the precedence of package 31 (CP-FR-003): `UPSTREAM_GIT_PAT` set and not empty; otherwise `UPSTREAM_GIT_USERNAME` and `UPSTREAM_GIT_PASSWORD` both set and not empty; otherwise the origin resolution, which is package 10's `ResolveOriginCredential` (a non-empty `GIT_PAT`, else `GIT_USERNAME` with `GIT_PASSWORD`). Package 31 sends a token to git as the password of the user `x-token-auth`; the forge gets the token itself.
4. **Unreadable secrets.** `500 internal`, nothing sent. The PRD is silent for the forge; package 10 states the same rule for origin.
5. **Forge kind matching ignores case.** The setting and the host are compared in lower case.
6. **`target` on the diagnostic route is ignored**, like any unknown query parameter; the route "takes no `target`".
7. **Parent access for a fork is part of the access step**, so its `403` comes before the `target` check. The check uses the user's level on the parent and does not run the binding guard against the parent's slug.
8. **How the token is sent.** `Authorization: Bearer <token>` on both forges; GitLab accepts it for personal, project and OAuth tokens. GitHub requests also carry `Accept: application/vnd.github+json`.
9. **The retry and the deadline.** The wait fits when it is at most the time left of the 30 seconds of the request that was rate limited. The retry is a new request with its own 30-second deadline. A route that needs several forge requests gives each its own deadline. The caller's `Retry-After` is the wait of the last rate-limited answer, rounded up to whole seconds. The deadline, the size limit and the waiting are constructor parameters with the fixed production values, so tests need not wait.
10. **Proxy variables are read once, when the forge client is built.** The standard library's own reader caches its first reading for the whole process, which tests could not vary; `golang.org/x/net/http/httpproxy` is used.
11. **Forge message.** GitHub's `message` member; GitLab's `message`, else `error`, a non-string value in its JSON form; empty when the body has neither.
12. **Cursors.** A forge cursor holds the next forge page, the page size and, for check runs, the head (commit id on GitHub, pipeline id on GitLab) the first page was read from, with package 02's fingerprint of route, path parameters and query parameters. A request with a cursor uses the cursor's page size; a `limit` given with it must equal that size, else `400`. This is stricter than the general list convention of package 02 and is kept as a decision of this package: a forge pages by page number and page size, so a page size that changed between two pages would skip or repeat entries. The delivery log of package 43 is read from the hub's store and follows the general convention. This is how `comments_next_cursor` (size 100) continues on the comments route. `next_cursor` is `null` when the forge names no next page (`Link` header, `rel="next"`).
13. **A page may hold fewer than `limit` items.** The hub drops pull requests from GitHub's issue list and system notes from GitLab's notes after the forge has paged, and does not refill.
14. **Empty label names.** A `labels` value with an empty name (`a,,b`, or empty) is `400`.
15. **Absent text.** `body` and `summary` are `""` when the forge gives none. A comment's `id` is the forge's numeric id.
16. **Mappings the PRD leaves to the forge client** (recorded so both forges answer alike):
    - *repository.* GitHub: `permissions` absent means `{pull: true, push: false, admin: false}`; the `allow_*` members as given, `null` when absent. GitLab: `full_name` is `path_with_namespace`; `private` is `visibility != "public"`; `pull` is `true`, `push` is access level ≥ 30 and `admin` ≥ 40, taking the higher of project and group access; `allow_merge_commit` is `merge_method == "merge"`, `allow_rebase_merge` is `merge_method` `rebase_merge` or `ff`, `allow_squash_merge` is `squash_option != "never"`, each `null` when the member is absent.
    - *state.* GitLab `opened` and `locked` are `open`. A pull request is `merged` when the forge says it was merged (GitHub `merged: true` or `merged_at` set).
    - *changed file.* GitHub `removed` is `deleted`, `copied` is `added`, `changed` and `unchanged` are `modified`. GitLab: `new_file`, `deleted_file`, `renamed_file`, else `modified`; `additions` and `deletions` count the lines of the file's `diff` that begin with `+` and `-`.
    - *check run.* GitHub: `waiting`, `requested` and `pending` are `queued`; conclusions `timed_out`, `action_required` and `startup_failure` are `failure`, `stale` is `cancelled`; `summary` is `output.summary`; `url` is `html_url`. GitLab job: `running` is `in_progress`; `success`, `failed`, `canceled` and `skipped` are `completed` with `success`, `failure` (`neutral` when `allow_failure` is true), `cancelled`, `skipped`; every other status is `queued`.
    - *review.* GitHub: the state in lower case; `pending` reviews are left out. GitLab: the forge reports no time for an approval, so approvals come first, in the forge's order, with `submitted_at: null` and `body: ""`; notes follow, oldest first. Approvals fill pages first; the page on which they end is filled up with notes, and notes are then read in pages of the size that page had left (`limit` when the approvals ended exactly on a page boundary or there are none).
    - *comment URL on GitLab.* The note's issue web URL followed by `#note_<id>`, built from the API URL's scheme and host and the repository path.
17. **The `webhook` member and package 43.** This package computes `webhook.configured` and `webhook.url` itself. `configured` is true when the workspace secret `FORGE_WEBHOOK_SECRET` is present and not empty, read through package 39's `Get`: the test the ingress of package 43 applies, so the document and the ingress agree; the value is used for nothing else and appears in no response, and a secret that cannot be read answers `500 internal` like a credential. `webhook.last_delivery_at` comes from a provider that package 43 registers with the forge component through `SetLastDeliveryProvider` in `service.go`; the provider is a function of a context and a workspace slug that returns the `received_at` of the newest recorded delivery, whether there is one, and an error. With no provider registered, or when it reports none, the member is `null`; a provider error answers `500 internal`. Package 43 registers the provider and does not change this route's handler.
18. **No per-workspace data.** This package stores nothing, so it registers no remover with package 14's deletion registry and no maintenance task.
19. **Profile: full.**

## Dependencies

- **10 workspace_create_and_clone** (draft): the workspace record (`git_url`, `workspace_mode`, `upstream_url`, kind, parent), the settings reader and the origin credential resolution.
- **39 secrets** (draft): reads the workspace secrets a credential is made of and whether `FORGE_WEBHOOK_SECRET` exists.
- **09 cli_foundation** (draft): the HTTP client, output and exit codes the ten commands are built on.
- **11 workspace_read_update_settings** (draft): registers the settings `forge_api_url` and `upstream_forge_api_url` and the `PATCH` that sets them.
- Through those: 02 (routes, error envelope, lists), 03 (scrubber, process composition), 06 (scope check), 07 (access component), 01 (build version).
- External: the REST APIs of GitHub (v3) and GitLab (v4).

## Verified External API

- `github.com/spf13/cobra` — **unverified** (not in `go.mod`): `cobra.Command` with `Use`, `Args`, `RunE func(cmd *cobra.Command, args []string) error`; `(*Command).AddCommand(cmds ...*Command)`; `(*Command).Flags() *pflag.FlagSet`.
- `golang.org/x/net/http/httpproxy` — **unverified** (not in `go.mod`): `httpproxy.FromEnvironment() *httpproxy.Config`; `(*Config).ProxyFunc() func(reqURL *url.URL) (*url.URL, error)`.
- Forge endpoints — **unverified** (written from knowledge of the public APIs, not read here; the implementer confirms each against the forge's documentation). `<p>` is the URL-encoded `owner/name`.

| Read | GitHub | GitLab |
|---|---|---|
| repository | `GET /repos/{owner}/{name}` | `GET /projects/<p>` |
| issue list | `GET /repos/{owner}/{name}/issues` with `state`, `sort`, `direction`, `labels`, `assignee`, `per_page`, `page`; entries with a `pull_request` member are pull requests | `GET /projects/<p>/issues` with `state` (`opened`, `closed`; left out for `all`), `order_by` (`created_at`, `updated_at`), `sort` (`asc`, `desc`), `labels`, `assignee_username`, `per_page`, `page` |
| issue | `GET …/issues/{number}` | `GET /projects/<p>/issues/{iid}` |
| comments | `GET …/issues/{number}/comments` | `GET /projects/<p>/issues/{iid}/notes?sort=asc&order_by=created_at`; entries with `system: true` are dropped |
| pull request | `GET …/pulls/{number}` | `GET /projects/<p>/merge_requests/{iid}` |
| changed files | `GET …/pulls/{number}/files` | `GET /projects/<p>/merge_requests/{iid}/diffs` |
| check runs | `GET …/commits/{head_sha}/check-runs` (`check_runs` member) | `GET /projects/<p>/merge_requests/{iid}/pipelines` (first entry is the latest), then `GET /projects/<p>/pipelines/{id}/jobs` |
| reviews | `GET …/pulls/{number}/reviews` | `GET /projects/<p>/merge_requests/{iid}/approvals` (`approved_by[].user`) and `GET /projects/<p>/merge_requests/{iid}/notes?sort=asc&order_by=created_at` |

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Origin**: A registered workspace's remote (`git_url`).
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires.
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Upstream**: For a carry-patch workspace, the project it tracks (`upstream_url`).
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Forge**: GitHub or GitLab, the service that hosts a workspace's remote repository.
- **Remote target**: The repository a forge route works on, selected by the query parameter `target`: `origin` or `upstream`.
- **Forge kind**: `github` or `gitlab`, decided from the API URL setting and the remote's host.
- **Credential kind**: What the hub sends to the forge for a target: `pat`, `basic` or `none`.
