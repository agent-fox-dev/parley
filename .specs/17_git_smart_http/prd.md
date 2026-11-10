---
spec_id: "17"
spec_name: "git_smart_http"
title: "Serve workspaces to stock git over HTTP"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "33b8ce43963e4c2eb17c24ffc3c1e64416a18a834998f4ff837e45234ad84c4f"
schema_version: 3
source: "docs/prd/05_git_server.md"
---

# Serve workspaces to stock git over HTTP

## Intent

The hub serves every workspace and every task fork as a git remote over smart HTTP, on the same port as the REST API, so that people and agents clone and fetch with an unmodified `git` and their hub credential. A git request is authenticated and authorized by the same rules as a REST request, a caller without access learns nothing about what exists, and `git` can obtain the hub credential from `parley credential-helper` without ever offering it to another host.

## Goals

- Exactly three routes outside `/api/v1` speak git smart HTTP: `GET /git/:org/:slug.git/info/refs`, `POST /git/:org/:slug.git/git-upload-pack` and `POST /git/:org/:slug.git/git-receive-pack`.
- A stock `git clone <external_url>/git/<org>/<slug>.git` with an API key, a PAT or the admin token as the HTTP Basic password succeeds and checks out the default branch; a task fork is cloned the same way with its own slug.
- Every refusal before the git exchange starts is an HTTP status with a one-line `text/plain` body, in the fixed check order: authenticate, rate limit, binding guard, PAT scope, repository resolution and access, request validation, workspace state.
- Unknown slug, wrong org, no org, missing `.git` and a caller without read access get byte-identical `404 repository not found` answers in every workspace status.
- Fetch works under protocol version 2 and version 0, with partial clone (`blob:none`, `blob:limit=<n>`, `tree:<n>`), shallow fetch (`--depth`, `--deepen`, `--shallow-since`, `--shallow-exclude`, `--unshallow`), incremental fetch and gzip request bodies.
- Every ref is advertised, including `refs/parley/**`; a task fork advertises its tracking branches at the parent's current heads and the version refs of its own changes.
- Every git request is counted once in `parley_git_requests_total{service,result}`, and a fetch that sends a pack is recorded as a `parley.git.fetch` event when the workspace setting `audit_fetches` is true.
- `parley credential-helper get` prints the hub credential only for the hub's own scheme, host and port.

## Non-goals

- Push processing, atomicity, the ordered push rules, compare-and-swap of refs, the `parley.git.push` event, `[git].max_push_size` and the timeout of a push: package 18 git_push_rules. This package routes, authenticates and authorizes `git-receive-pack`, serves its ref advertisement, and hands the request body to the push processor that package 18 supplies.
- Mapping a push to a change, the `parley.*` push options and the `remote:` lines of a push: package 19 git_push_to_change.
- The credential check itself (formats, hashes, revocation, expiry, blocking): package 04 authentication_and_login. The scope registry and implication rules: package 06 api_keys_and_access_tokens.
- The workspace access rule and the workspace record it reads: package 07 workspace_access_rule. The binding guard's coverage rule and the `parley.token.bound_refused` event: package 08 workspace_bound_tokens.
- The rate limiter's counting rule and window: package 02 http_api_conventions.
- Creating a workspace's bare repository, its `HEAD` and its place on disk, and the workspace state guard: package 10 workspace_create_and_clone. The maintenance marker of reclone, archive and delete: package 14 workspace_archive_reclone_delete.
- Registering the setting `audit_fetches` and the `hub_url` member of the workspace object: package 11 workspace_read_update_settings.
- Creating a task fork, recording which refs of the fork are tracking refs, and creating the fork's repository: package 15 task_fork_creation. Recording which changes have a fork as their source and writing `refs/parley/changes/<number>/<version>`: package 22 change_opening_and_versions.
- The Prometheus exposition at `/metrics`: package 51 audit_query_and_retention. The event log: package 52 durable_event_log_and_stream.
- The client settings and the configuration file the credential helper reads: package 09 cli_foundation.
- SSH transport and the dumb HTTP protocol: not offered by the product.

## Background

The repository holds no code yet; every file named here is new, except files earlier packages create and this package extends (`cmd/parleyd/main.go`, `internal/gitrepo/`, `internal/cli/root.go`).

The source is `docs/prd/05_git_server.md`: GIT-FR-001 to GIT-FR-016 (routing, authentication, authorization, protocol), GIT-FR-070 to GIT-FR-073 (credential helper), the fetch side of GIT-FR-062 (metrics), the `parley.git.fetch` row of §5.8, the refusals table of §5.5, the entry of §5.6 and the fetch-side cases of §5.10. It rests on `docs/prd/03_platform.md` PLAT-FR-013 (check order), PLAT-FR-020 (authentication), PLAT-FR-025 (rate limit), PLAT-FR-061 (anti-enumeration) and PLAT-FR-083 (binding guard), and on `docs/prd/04_workspaces_and_forks.md` §4.3.6 (tracking branches) and WS-FR-066 (a fork under an archived or busy parent).

Earlier packages supply what a git request needs: the credential check (`internal/identity`, package 04), the scope check (package 06), the access decision without an HTTP response (`internal/access`, package 07), the limiter (`internal/server`, package 02), the workspace record, its status, its state guard, its settings document and the place of its bare repository (`internal/workspace`, package 10), and the `git` subprocess wrapper (`internal/gitrepo`, package 10).

The code of this package lives in `internal/gitserver/` (routes, checks, the fetch service, the fork view, metrics, the fetch event), `internal/gitrepo/` (running `git upload-pack` and `git receive-pack` in stateless mode, copying refs between repositories), `internal/cli/credential_helper.go` (the command), `cmd/parleyd/main.go` (wiring) and `test/e2e/git_test.go` (stock `git` against a real hub).

## Requirements

### Routes and framing

GIT-FR-001, GIT-FR-002, GIT-FR-009, §5.5. Three routes exist under `/git`. They are outside `/api/v1`, take HTTP Basic authentication and never use the JSON error envelope.

- `GET /git/:org/:slug.git/info/refs?service=<service>` answers `200` with `Content-Type: application/x-<service>-advertisement` and `Cache-Control: no-cache`. `service` is required and must be `git-upload-pack` or `git-receive-pack`; when the parameter is repeated the first value counts, so `service=git-upload-pack&service=git-receive-pack` is upload-pack. A missing or other value is `400` `invalid or missing service parameter`.
- `POST /git/:org/:slug.git/git-upload-pack` answers `200` with `Content-Type: application/x-git-upload-pack-result` and `Cache-Control: no-cache`, streamed.
- `POST /git/:org/:slug.git/git-receive-pack` runs the same checks with need write and hands the request to the push processor (package 18).

The REST body limit `[server].max_body_size` and the REST content-type rule do not apply to these routes. An error before the git exchange starts is the HTTP status with `Content-Type: text/plain; charset=utf-8` and a body of one line of text, which git prints as `remote: <message>`. A lookup or repository failure before the exchange starts is `500` `internal error`. Once the exchange has started the status is `200` and errors are in-band `ERR` packets.

### Authentication

GIT-FR-004, PLAT-FR-020, PLAT-FR-025. The credential is the Basic password: an API key, a PAT or the admin token. The Basic user name is never consulted. Bearer tokens are not accepted on git routes.

- No `Authorization` header, a scheme other than `Basic`, undecodable Basic data, an empty password, and an unknown, revoked or expired credential: `401` with `WWW-Authenticate: Basic realm="parley"` and the body `authentication required`, so that git asks its credential helper. All of them have the same body.
- A valid credential of a blocked user: `403` `user is blocked`.
- A request over the rate limit of its credential: `429` with `Retry-After` in whole seconds and the body `rate limit exceeded`.

### Authorization and repository resolution

GIT-FR-003, GIT-FR-005, GIT-FR-006, GIT-FR-008, §5.10. Checks run in this order and the first failing one decides the answer: authenticate → rate limit → binding guard (bound PATs only) → PAT scope → repository resolution and access → request validation → workspace state → act.

- **Need.** `git-receive-pack`, and `info/refs` with `service=git-receive-pack`, need write access and the scope `git:write`. Everything else needs read access and `git:read`; `git:write` satisfies a `git:read` check. Only PATs are scope-checked.
- **Binding guard.** A bound PAT on a repository its binding does not cover: `404` `repository not found`. The guard is the git guard of package 08 (`GuardGit`), which reports its refusal with the surface `git`.
- **Scope.** A PAT without the needed scope: `403` `token lacks scope git:read` or `token lacks scope git:write`, whether or not the repository exists.
- **Resolution.** The repository resolves only when the path segment ends in `.git`, the slug names a workspace, the workspace has an organization and `:org` is that organization's slug. A segment without `.git`, an unknown slug, a workspace without an organization, a wrong org and a caller without read access all get `404` `repository not found`, with the same bytes, whatever the workspace's status.
- **Access.** The admin token and the workspace's owner fetch and push. An org member who is not the owner fetches; on a push route the member gets `403` `org members have read-only access to this workspace; create a task fork to contribute`.
- Membership and token bindings are read on every request: a member removed from the organization is a stranger on the next request.

### Workspace state

GIT-FR-007, WS-FR-066. For a caller who passed access:

- A workspace whose status is not `ready`: `409` `repository not available: workspace is <status>`.
- A task fork whose parent is in exclusive maintenance: `409` `repository not available: parent workspace is busy`, on all three routes.
- A push route on a task fork whose parent is `archived`: `409` `repository is read-only: parent workspace is archived`. Fetches from that fork keep working.

### Fetch protocol

GIT-FR-010, GIT-FR-012, GIT-FR-013, §5.5, `[git].timeout`. Fetch uses protocol version 2 when the request carries `Git-Protocol: version=2`, and version 0 otherwise. Shallow fetch works with `--depth`, `--deepen`, `--shallow-since`, `--shallow-exclude` and `--unshallow`. An incremental fetch negotiates haves and wants and sends only missing objects. A request body with `Content-Encoding: gzip` is accepted. Progress is streamed over the side band.

In-band errors, with HTTP `200`: an empty request body, a body that is not a git request, an invalid gzip body, and a fetch that runs longer than `[git].timeout` (default `30m`), whose git process is stopped.

### Partial clone

GIT-FR-011. The filters `blob:none`, `blob:limit=<n>` and `tree:<n>` are accepted, and a client later fetches the omitted objects on demand. Every other filter kind is refused with an in-band error.

### Ref advertisement

GIT-FR-014, GIT-FR-015, §5.3. Every ref is advertised: branches, tags, notes and everything under `refs/parley/`. Hub-owned refs are fetchable by name; a default clone does not fetch them, because git's default refspec covers only branches and tags. Under version 2 the client's ref-prefix filter limits the listing. A client may request by object id any commit reachable from an advertised ref; an id that is not reachable is an in-band error. `HEAD` names the default branch, so a clone checks it out.

In a task fork, the tracking refs are advertised at the parent's current values: when the parent's branch moves, the fork's advertisement moves with it with no sync call; when the parent's branch is deleted, it disappears from the fork; a branch created in the parent after the fork does not appear. The fork also advertises, under the same names as in the parent, the version refs `refs/parley/changes/<number>/<version>` of the changes whose source is the fork, and no version ref of any other change.

### Push advertisement and hand-over

GIT-FR-016, GIT-FR-003. `info/refs` with `service=git-receive-pack` advertises the refs with at least the capabilities `report-status`, `side-band-64k`, `delete-refs`, `ofs-delta`, `atomic` and `push-options`. A `POST …/git-receive-pack` that passed every check is handed to the push processor with the caller facts (credential type, credential id, user, access level, binding, and the PAT record with its `paths`) and the workspace record. The processor writes the whole response and returns the result `ok`, `refused` or `error`, which this package counts.

### Metrics and the fetch event

GIT-FR-062, §5.8, §5.9. Every git request adds one to `parley_git_requests_total{service,result}`, with `service` `info-refs`, `upload-pack` or `receive-pack` and `result` `ok`, `refused` (an answer of `401`, `403` or `404`) or `error`.

When the workspace setting `audit_fetches` is true, each served upload-pack request that sends a pack emits one `parley.git.fetch` event: `resource_type` `workspace`, `resource_id` and `workspace` the slug (the fork's slug for a fork), `action` `fetch`, the actor of the credential with `actor_type` `pat`, `api_key` or `admin_token`, and metadata `shallow` (boolean) and `filter` (the filter text or null). Ref discovery, a negotiation round without a pack and every request while the setting is false emit nothing. Path limits of a token never restrict fetch.

### Credential helper

GIT-FR-070 to GIT-FR-073, §5.6. `parley credential-helper <verb>` implements git's credential helper protocol and makes no request to the hub. It takes exactly one argument; any other count is a usage error with exit status 2.

- Only `get` produces output. `store`, `erase` and any other verb print nothing and exit 0.
- The hub URL and the credential come from the CLI's settings (the configuration file in `~/.parley` or `PARLEY_CONFIG_DIR`, overridden by `PARLEY_ENDPOINT_URL` and `PARLEY_API_KEY`). When either is missing or the URL has no host, the helper prints nothing and exits 0.
- It reads `key=value` lines from stdin up to the first empty line. It answers only when `protocol` equals the hub URL's scheme and the host and port match: hosts compare without regard to case, and a missing port means 443 for `https` and 80 for `http`. The path is not considered.
- On a match it prints exactly four lines: `protocol=<protocol>`, `host=<host as given>`, `username=x-token-auth`, `password=<credential>`. Example: a hub URL `https://hub` matches a request for `host=HUB:443`; an `http` request for an `https` hub gets no answer.

Setup: `git config --global credential.<hub-url>.helper '!parley credential-helper'`.

### End-to-end flows

1. **Clone with the credential helper.** A developer configures the helper and runs `git clone <hub>/git/<org>/<slug>.git`. The git client asks `info/refs` without a credential; `parleyd` answers `401` with the Basic challenge; the git client runs `parley credential-helper get`; the parley CLI prints the credential for the hub's host; the git client repeats the request with Basic; `parleyd` authenticates, resolves the repository, checks the state and advertises the refs; the git client posts to `git-upload-pack`; `parleyd` streams the pack; the clone has the default branch checked out.
2. **An agent works from a task fork.** An agent with a PAT bound to a fork runs a shallow, blob-less clone of the fork. `parleyd` passes the guard and the scope check, brings the fork's tracking refs to the parent's current heads and serves the fetch; with `audit_fetches` true it records `parley.git.fetch` with `shallow` true and `filter` `blob:none`. The same token on the parent's git URL gets `404 repository not found`. The agent fetches `refs/parley/changes/<number>/<version>` of its own change from the fork.

## Design Decisions

1. **Project decisions relied on.** Go; the server needs `git` 2.38 or later; repositories are bare repositories on disk; git runs as a subprocess through `internal/gitrepo`, with no git library; HTTP is `net/http` with its method-and-pattern router; the CLI is `github.com/spf13/cobra`; tests use `testing`, table-driven, with `net/http/httptest`, and repositories made with `git init` in `t.TempDir()`.
2. **The exchange is git's own.** `parleyd` runs `git upload-pack --stateless-rpc` and `git receive-pack --stateless-rpc --advertise-refs` on the bare repository and frames their output as smart HTTP. Protocol version 2 is passed to git through the `GIT_PROTOCOL` environment variable only when the `Git-Protocol` header has the entry `version=2`. Why: the PRD asks for the behaviour of stock git on both sides.
3. **Git configuration per run, not per repository.** Each run passes `uploadpack.allowFilter=true`, `uploadpack.allowReachableSHA1InWant=true`, `uploadpackfilter.allow=false` with `blob:none`, `blob:limit` and `tree` allowed, `receive.advertisePushOptions=true` and `receive.advertiseAtomic=true` on the command line, hides no ref, and ignores the system and global git configuration. Why: a copy made by package 10 needs no change, and the hub's answer does not depend on the host's configuration.
4. **Other filter kinds are refused.** The PRD says they "may be refused"; refusing gives one fixed answer.
5. **Paths under `/git` that are none of the three routes**, and other methods on the three, get the server's general `404` and `405` answers of package 02, before authentication. A path of the right shape whose repository segment lacks `.git` is one of the three routes and gets `404 repository not found` after authentication, as §5.5 lists it. The org segment is compared with the organization's slug exactly, with regard to case.
6. **Refusal bodies** are `text/plain; charset=utf-8`, the message followed by one newline. Every `401` has the body `authentication required`; the four REST messages of PLAT-FR-020 are not used on git (§5.5).
7. **Rate limit.** A request that failed authentication is counted against its client address and answered `401` (package 02, decision 11); an authenticated request is counted against its credential.
8. **The fork view (`ForkView`), the push processor (`PushProcessor`) and the event sink are interfaces declared in `internal/gitserver`; the guard is `GuardGit` of package 08.** `parleyd` passes the guard of package 08, a fork view built from packages 15 and 22, the push processor of package 18 and the event log of package 52. The `ForkView` is wired in `internal/hub/gitserver.go`, the hub's composition file for the git server (with packages 15, 18 and 22), not in `cmd/parleyd`. Why: packages 15, 18 and 22 are built after this one; packages 08, 09 and 52 are dependencies. Without a guard `parleyd` refuses every bound PAT with `404 repository not found`; without a fork view a fork is served with the refs its repository holds; without a push processor `POST …/git-receive-pack` answers `500 internal error` and changes nothing; without an event sink no fetch event is written. None of them fails open.
   **The hand-off to package 18 is `PushProcessor`, declared in `internal/gitserver/push.go`.** It takes the response writer, the request with its unread body, the caller facts (credential type, credential id, user, access level `owner` or `admin`, the guard's binding, the PAT record with its `paths`) and the workspace record; it writes the whole response and returns `ok`, `refused` or `error`. Package 18 implements exactly this interface and never increments the request counter itself.
9. **How a fork's advertisement is made.** Before git runs on a fork's repository, `parleyd` asks the fork view for the fork's tracking ref names (full names, branches and tags) and the numbers of the changes whose source is the fork, and copies those refs from the parent's repository into the fork's with a local fetch: a tracking ref is set to the parent's current value or deleted when the parent no longer has it, and every `refs/parley/changes/<number>/*` of those changes is copied. A version ref in the fork's repository that belongs to no such change is deleted. Why: stock `git upload-pack` then advertises the PRD's view, and the copy transfers nothing when the fork borrows the parent's objects. The tracking names are the branch and tag names package 15 recorded on the fork at creation; the change numbers come from package 22, which writes `refs/parley/changes/<number>/<version>` in the target workspace, for a fork source the fork's parent, so the parent's repository is where they are copied from. The copies are a cache for stock git: the ref view of package 15 stays the one answer to which refs a fork has, and it lists a recorded tracking name once, as a tracking ref, also when the copy exists in the fork's repository.
10. **Exclusive maintenance of a parent** is what the state guard of package 10 reports for the parent's record. A fork's own status is checked first, then the busy parent, then the archived parent on a push route.
11. **In-band error texts of the hub**: `empty request`, `invalid gzip body` and `request timed out`, each as one `ERR` packet. A request git itself rejects carries git's own text. On a timeout the git process is killed and the response ends; the `ERR` packet is written only when no pack data has been written yet.
12. **`[git].timeout`** is registered by this package with the configuration loader (duration, default `30m`, must be positive) and bounds one upload-pack request. Package 18 applies the same key to a push and registers `[git].max_push_size`.
13. **Metric result.** `refused` is exactly an answer of `401`, `403` or `404`, as GIT-FR-062 lists it. `error` is every other answer that is not a served exchange (`400`, `409`, `429`, `500`) and an exchange that ended with a hub `ERR` packet, a failed git process or a timeout. `ok` is everything else. For a push handed over, the processor returns the result and this package counts it, once. The counter lives in `internal/gitserver`; package 51 exports it.
14. **`audit_fetches`** is read from the workspace's settings document (package 10). While no package has registered the key, it is false.
15. **A pack was sent** when the response carries pack data: under version 2 a `packfile` section, under version 0 a payload that starts with `PACK`. **`shallow`** is true when the request carries a `deepen`, `deepen-since` or `deepen-not` line. **`filter`** is the filter text of the request, or null.
16. **A failure to write the fetch event** is logged and never fails the fetch, as GIT-FR-060 rules for push events.
17. **Credential helper details.** It resolves its settings as every command does (package 09), so the global flags count too. An unreadable configuration file, a hub URL whose scheme is neither `http` nor `https`, and a request without `protocol` or `host` give no output and exit 0, because git must go on to its next helper. A line without `=` is ignored. The wrong argument count is reported as the CLI's `usage_error` on stderr.
18. **Profile: full.**

## Dependencies

- **10 workspace_create_and_clone** (draft): the workspace record with its status, organization, default branch and settings document, the state guard, the place of the bare repository, and `internal/gitrepo`.
- **06 api_keys_and_access_tokens** (draft): the PAT with its scopes and `workspace` member, and the scope check with its implication rules (`git:write` implies `git:read`).
- **07 workspace_access_rule** (draft): the access decision without an HTTP response, with the level and the record (kind, parent, org).
- **08 workspace_bound_tokens** (draft): the git guard `GuardGit`, which runs on the three git routes.
- **09 cli_foundation** (draft): the CLI's settings, output and errors, on which `parley credential-helper` is built.
- **52 durable_event_log_and_stream** (draft): the event log that receives `parley.git.fetch`.
- Through those: 02 http_api_conventions (the router and the limiter), 03 server_lifecycle_and_configuration (the configuration loader), 04 authentication_and_login (the credential check).
- Called through interfaces of this package, not dependencies: 15 task_fork_creation and 22 change_opening_and_versions (the fork view), 18 git_push_rules (the push processor), 51 audit_query_and_retention (metric export).
- External: the `git` command, 2.38 or later, on the server; stock `git` as the client.

## Verified External API

- `github.com/spf13/cobra` v1.10.2, read in the module cache: `func ExactArgs(n int) PositionalArgs`, `func (c *Command) AddCommand(cmds ...*Command)`, `func (c *Command) InOrStdin() io.Reader`, `func (c *Command) OutOrStdout() io.Writer`.
- The `git` command, tried with git 2.56.0 on a bare repository: `git upload-pack --stateless-rpc --advertise-refs <directory>` prints the version 0 advertisement with `HEAD`, the `symref=HEAD:refs/heads/<branch>` capability, `filter` and every ref including `refs/parley/…`; with `GIT_PROTOCOL=version=2` it prints `version 2` and the capabilities `ls-refs` and `fetch=shallow wait-for-done filter`; `git -c receive.advertisePushOptions=true -c receive.advertiseAtomic=true receive-pack --stateless-rpc --advertise-refs <directory>` advertises `report-status`, `delete-refs`, `side-band-64k`, `atomic`, `ofs-delta` and `push-options`. The keys `uploadpackfilter.*` and `uploadpack.allowReachableSHA1InWant` are **unverified** in their effect; task 5 confirms them against the installed `git`.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task. It shares history with the parent, has its own branches and expires: after 7 days by default, or 24 hours after all its changes have landed or been abandoned.
- **Tracking branch**: In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch.
- **Version**: One head of a change's source branch. Every push, API commit or restack that moves the source creates a new version. Old versions stay fetchable.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it). A token bound to a fork also reaches its own changes, and the awareness and policy reads, on the fork's parent (§3).
- **Hub-owned ref**: A ref under `refs/parley/`, written only by the hub.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
