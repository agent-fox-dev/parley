# af-hub REST API Reference

This is the reference of the hub's HTTP API: every route a client can call,
what it takes, what it answers and how it fails. It is for people writing
clients of the hub (agents, scripts, the `afc` CLI) and for operators checking
what a call does. Routes live under `/api/v1`, apart from the few
[server endpoints](#server-endpoints), the [git server](#git-server-endpoints)
and the [forge webhook ingress](#the-webhook-ingress).

Neighbouring documents: [openapi.yaml](openapi.yaml) is a machine-readable copy
of this reference (where the two disagree, the code decides; see
[ADR 10](adr/10-follow-apikit-and-the-code-where-the-specs-disagree.md));
[cli.md](cli.md) describes the `afc` commands that call these routes;
[permissions.md](permissions.md) explains the permission scopes;
[configuration.md](configuration.md) the server settings; and
[architecture.md](architecture.md) how the hub works inside.

## Contents

**General**

- [Conventions](#conventions): base URL, path parameters, bodies, times, paging
- [Authentication](#authentication): API keys, PATs and admin tokens
- [Permission Scopes](#permission-scopes) ([implied permissions](#implied-permissions))
- [Workspace Access](#workspace-access): owner, member and admin; read, write and private routes ([anti-enumeration](#anti-enumeration-policy))
- [Workspace-Bound Tokens](#workspace-bound-tokens)
- [Error Response Schema](#error-response-schema): [common answers](#common-answers), [error types](#error-types)
- [Workspace Lock](#workspace-lock): `409 workspace_busy`
- [Workspace Response Schema](#workspace-response-schema)
- [Workspace Settings Reference](#workspace-settings-reference)
- [Hub Audit Events](#hub-audit-events)

**Workspaces** ([section](#workspace-endpoints))

| Route | Purpose |
|-------|---------|
| [`POST /api/v1/workspaces`](#post-apiv1workspaces) | Create a workspace |
| [`GET /api/v1/workspaces`](#get-apiv1workspaces) | List workspaces |
| [`GET /api/v1/workspaces/:slug`](#get-apiv1workspacesslug) | Read a workspace |
| [`PATCH /api/v1/workspaces/:slug`](#patch-apiv1workspacesslug) | Change a workspace and its settings |
| [`POST /api/v1/workspaces/:slug/archive`](#post-apiv1workspacesslugarchive) | Archive a workspace |
| [`POST /api/v1/workspaces/:slug/reactivate`](#post-apiv1workspacesslugreactivate) | Reactivate an archived workspace |
| [`POST /api/v1/workspaces/:slug/sync`](#post-apiv1workspacesslugsync) | Sync the clone with its remote ([carry-patch sync](#carry-patch-sync-extension)) |
| [`POST /api/v1/workspaces/:slug/reclone`](#post-apiv1workspacesslugreclone) | Delete the clone and clone again |
| [`POST /api/v1/workspaces/:slug/branches/:name/push`](#post-apiv1workspacesslugbranchesnamepush) | Push one branch to `origin` |
| [`DELETE /api/v1/workspaces/:slug`](#delete-apiv1workspacesslug) | Delete an archived workspace |

**Bound tokens** ([section](#workspace-bound-token-endpoints))

| Route | Purpose |
|-------|---------|
| [`POST /api/v1/workspaces/:slug/tokens`](#post-apiv1workspacesslugtokens) | Mint a PAT bound to the workspace |
| [`GET /api/v1/workspaces/:slug/tokens`](#get-apiv1workspacesslugtokens) | List the workspace's bound tokens |

**Agent sessions** ([section](#agent-session-endpoints))

| Route | Purpose |
|-------|---------|
| [`POST /api/v1/sessions`](#post-apiv1sessions) | Open a session |
| [`POST /api/v1/sessions/:id/complete`](#post-apiv1sessionsidcomplete) | Close a session |
| [`POST /api/v1/sessions/:id/usage`](#post-apiv1sessionsidusage) | Report token usage |
| [`GET /api/v1/sessions`](#get-apiv1sessions) | List sessions |
| [`GET /api/v1/sessions/:id`](#get-apiv1sessionsid) | Read a session |
| [`GET /api/v1/sessions/:id/usage`](#get-apiv1sessionsidusage) | List a session's usage |
| [`GET /api/v1/workspaces/:slug/cost`](#get-apiv1workspacesslugcost) | A workspace's token usage over a period |

**Patches** ([section](#patch-endpoints), [schema](#patch-response-schema))

| Route | Purpose |
|-------|---------|
| [`POST /api/v1/workspaces/:slug/patches`](#post-apiv1workspacesslugpatches) | Add patches |
| [`GET /api/v1/workspaces/:slug/patches`](#get-apiv1workspacesslugpatches) | List patches |
| [`GET /api/v1/workspaces/:slug/patches/:id`](#get-apiv1workspacesslugpatchesid) | Read a patch |
| [`PATCH /api/v1/workspaces/:slug/patches/:id`](#patch-apiv1workspacesslugpatchesid) | Change a patch |
| [`DELETE /api/v1/workspaces/:slug/patches/:id`](#delete-apiv1workspacesslugpatchesid) | Remove a patch |
| [`POST /api/v1/workspaces/:slug/patches/:id/restore`](#post-apiv1workspacesslugpatchesidrestore) | Restore a deleted patch |
| [`POST /api/v1/workspaces/:slug/patches/:id/reset-to-origin`](#post-apiv1workspacesslugpatchesidreset-to-origin) | Set a patch branch to the fork's tip |
| [`POST /api/v1/workspaces/:slug/patches/reorder`](#post-apiv1workspacesslugpatchesreorder) | Reorder the patch list |

**Rerere** ([section](#rerere-management-endpoints))

| Route | Purpose |
|-------|---------|
| [`GET /api/v1/workspaces/:slug/rerere`](#get-apiv1workspacesslugrerere) | List recorded conflict resolutions |
| [`DELETE /api/v1/workspaces/:slug/rerere/*pathspec`](#delete-apiv1workspacesslugrererepathspec) | Forget a resolution |

**Rebuilds** ([section](#rebuild-endpoints), [schema](#rebuild-job-response-schema))

| Route | Purpose |
|-------|---------|
| [`POST /api/v1/workspaces/:slug/rebuild`](#post-apiv1workspacesslugrebuild) | Queue a rebuild |
| [`GET /api/v1/workspaces/:slug/rebuilds`](#get-apiv1workspacesslugrebuilds) | List rebuilds |
| [`GET /api/v1/workspaces/:slug/rebuilds/:id`](#get-apiv1workspacesslugrebuildsid) | Read a rebuild |
| [`DELETE /api/v1/workspaces/:slug/rebuilds/:id`](#delete-apiv1workspacesslugrebuildsid) | Cancel a queued rebuild |
| [`POST /api/v1/workspaces/:slug/rebuilds/:id/requeue`](#post-apiv1workspacesslugrebuildsidrequeue) | Requeue a dead-lettered rebuild |
| [`POST /api/v1/workspaces/:slug/rebuilds/:id/rollback`](#post-apiv1workspacesslugrebuildsidrollback) | Roll the integration branch back |
| [`GET /api/v1/workspaces/:slug/rebuild-preview`](#get-apiv1workspacesslugrebuild-preview) | Predict conflicts |

**Patch status** ([section](#patch-status-dashboard-endpoint))

| Route | Purpose |
|-------|---------|
| [`GET /api/v1/workspaces/:slug/patch-status`](#get-apiv1workspacesslugpatch-status) | Health of the patch stack |

**Merges** ([section](#merge-endpoints), [schema](#merge-job-response-schema))

| Route | Purpose |
|-------|---------|
| [`POST /api/v1/workspaces/:slug/merges`](#post-apiv1workspacesslugmerges) | Queue a merge |
| [`GET /api/v1/workspaces/:slug/merges`](#get-apiv1workspacesslugmerges) | List merges |
| [`GET /api/v1/workspaces/:slug/merges/:id`](#get-apiv1workspacesslugmergesid) | Read a merge |
| [`DELETE /api/v1/workspaces/:slug/merges/:id`](#delete-apiv1workspacesslugmergesid) | Cancel a queued merge |
| [`POST /api/v1/workspaces/:slug/merges/:id/requeue`](#post-apiv1workspacesslugmergesidrequeue) | Requeue a dead-lettered merge |
| [`POST /api/v1/workspaces/:slug/rebase`](#post-apiv1workspacesslugrebase) | Rebase several branches onto a target |

**Git server** ([section](#git-server-endpoints): [authentication](#git-server-authentication), [access](#git-access), [scopes](#git-permission-scopes), [push control](#pre-receive-hook-push-control), [`refs/hub/replaced/`](#protected-namespace-refshubreplaced), [after a push](#after-a-push))

| Route | Purpose |
|-------|---------|
| [`GET /git/:org/:slug.git/info/refs`](#get-gitorgsluggitinforefs) | Ref advertisement |
| [`POST /git/:org/:slug.git/git-upload-pack`](#post-gitorgsluggitgit-upload-pack) | Clone and fetch |
| [`POST /git/:org/:slug.git/git-receive-pack`](#post-gitorgsluggitgit-receive-pack) | Push |

**Secrets** ([section](#secrets-endpoints))

| Route | Purpose |
|-------|---------|
| [`POST /api/v1/user/secrets`](#post-apiv1usersecrets) · [`GET`](#get-apiv1usersecrets) · [`PATCH .../:key`](#patch-apiv1usersecretskey) · [`DELETE .../:key`](#delete-apiv1usersecretskey) | The caller's secrets |
| [`POST /api/v1/orgs/:slug/secrets`](#post-apiv1orgsslugsecrets) · [`GET`](#get-apiv1orgsslugsecrets) · [`PATCH .../:key`](#patch-apiv1orgsslugsecretskey) · [`DELETE .../:key`](#delete-apiv1orgsslugsecretskey) | An organization's secrets |
| [`POST /api/v1/workspaces/:slug/secrets`](#post-apiv1workspacesslugsecrets) · [`GET`](#get-apiv1workspacesslugsecrets) · [`PATCH .../:key`](#patch-apiv1workspacesslugsecretskey) · [`DELETE .../:key`](#delete-apiv1workspacesslugsecretskey) | A workspace's secrets |

**Variables** ([section](#variables-endpoints))

| Route | Purpose |
|-------|---------|
| [`POST /api/v1/user/vars`](#post-apiv1uservars) · [`GET`](#get-apiv1uservars) · [`PATCH .../:key`](#patch-apiv1uservarskey) · [`DELETE .../:key`](#delete-apiv1uservarskey) | The caller's variables |
| [`POST /api/v1/orgs/:slug/vars`](#post-apiv1orgsslugvars) · [`GET`](#get-apiv1orgsslugvars) · [`PATCH .../:key`](#patch-apiv1orgsslugvarskey) · [`DELETE .../:key`](#delete-apiv1orgsslugvarskey) | An organization's variables |
| [`POST /api/v1/workspaces/:slug/vars`](#post-apiv1workspacesslugvars) · [`GET`](#get-apiv1workspacesslugvars) · [`PATCH .../:key`](#patch-apiv1workspacesslugvarskey) · [`DELETE .../:key`](#delete-apiv1workspacesslugvarskey) | A workspace's variables |
| [`GET /api/v1/workspaces/:slug/vars/resolved`](#get-apiv1workspacesslugvarsresolved) | A workspace's variables merged over user, organization and workspace |

**Sandboxes** ([profiles](#sandbox-profile-endpoints), [leases](#sandbox-lease-endpoints), [lease representation](#the-lease-representation), [gateway](#the-sandbox-gateway))

| Route | Purpose |
|-------|---------|
| [`GET /api/v1/sandbox-profiles`](#get-apiv1sandbox-profiles) | List sandbox profiles |
| [`POST /api/v1/workspaces/:slug/sandboxes`](#post-apiv1workspacesslugsandboxes) | Issue a lease |
| [`GET /api/v1/workspaces/:slug/sandboxes`](#get-apiv1workspacesslugsandboxes) | List a workspace's leases |
| [`GET /api/v1/workspaces/:slug/sandboxes/:id`](#get-apiv1workspacesslugsandboxesid) | Read a lease |
| [`DELETE /api/v1/workspaces/:slug/sandboxes/:id`](#delete-apiv1workspacesslugsandboxesid) | End a lease and destroy its sandbox |
| [`GET /api/v1/sandboxes`](#get-apiv1sandboxes) | List every lease (admin) |

**Forge** ([section](#forge-endpoints): [how the hub reaches the forge](#how-the-hub-reaches-the-forge), [order of checks](#order-of-checks), [target](#the-target), [read routes](#the-read-routes), [write routes](#the-write-routes), [webhook routes](#the-webhook-routes), [failures](#failures-of-the-forge))

| Route | Purpose |
|-------|---------|
| [`GET /api/v1/workspaces/:slug/forge`](#get-apiv1workspacesslugforge) | How the hub reaches the forge |
| [`GET .../forge/repository`, `.../issues`, `.../issues/:n`, `.../issues/:n/comments`, `.../pulls/:n`, `.../pulls/:n/state`, `.../pulls/:n/files`, `.../pulls/:n/checks`, `.../pulls/:n/reviews`](#the-read-routes) | Read the repository, issues and pull requests |
| [`POST .../issues`, `PATCH .../issues/:n`, `POST .../issues/:n/close`, `POST .../issues/:n/comments`, `POST .../issues/:n/labels`, `DELETE .../issues/:n/labels/:name`, `POST .../labels`, `POST .../pulls`, `POST .../pulls/:n/comments`](#the-write-routes) | Change issues, comments, labels and pull requests |
| [`PUT /api/v1/workspaces/:slug/forge/webhook`](#put-apiv1workspacesslugforgewebhook) | Set the webhook secret |
| [`DELETE /api/v1/workspaces/:slug/forge/webhook`](#delete-apiv1workspacesslugforgewebhook) | Remove the webhook secret |
| [`GET /api/v1/workspaces/:slug/forge/deliveries`](#get-apiv1workspacesslugforgedeliveries) | List received deliveries |
| [`POST /hooks/forge/:slug`](#the-webhook-ingress) | Receive the forge's deliveries |

**Audit ingestion** ([section](#audit-ingestion-endpoints))

| Route | Purpose |
|-------|---------|
| [`POST .../runs/:run_id/events`](#post-apiv1workspacesslugrunsrun_idevents) · [`GET`](#get-apiv1workspacesslugrunsrun_idevents) · [`POST .../events/batch`](#post-apiv1workspacesslugrunsrun_ideventsbatch) | A run's audit events |
| [`POST .../runs/:run_id/sessions/outcomes`](#post-apiv1workspacesslugrunsrun_idsessionsoutcomes) · [`GET`](#get-apiv1workspacesslugrunsrun_idsessionsoutcomes) | Session outcomes |
| [`POST .../runs/:run_id/tools/calls`](#post-apiv1workspacesslugrunsrun_idtoolscalls) · [`GET`](#get-apiv1workspacesslugrunsrun_idtoolscalls) | Tool calls |
| [`POST .../runs/:run_id/tools/errors`](#post-apiv1workspacesslugrunsrun_idtoolserrors) · [`GET`](#get-apiv1workspacesslugrunsrun_idtoolserrors) | Tool errors |
| [`POST .../runs/:run_id/traces`](#post-apiv1workspacesslugrunsrun_idtraces) · [`GET`](#get-apiv1workspacesslugrunsrun_idtraces) · [`POST .../traces/batch`](#post-apiv1workspacesslugrunsrun_idtracesbatch) | Conversation traces |
| [`POST .../runs/:run_id/postmortem`](#post-apiv1workspacesslugrunsrun_idpostmortem) · [`GET`](#get-apiv1workspacesslugrunsrun_idpostmortem) | A run's postmortem |

(`...` is `/api/v1/workspaces/:slug`.)

**Audit query** ([section](#audit-query-endpoints))

| Route | Purpose |
|-------|---------|
| [`GET /api/v1/audit`](#get-apiv1audit) | Query hub and agent events |
| [`GET /api/v1/workspaces/:slug/runs/:run_id/transcript`](#get-apiv1workspacesslugrunsrun_idtranscript) | Rebuild a node's conversation |
| [`GET /api/v1/events`](#get-apiv1events) | Stream events live (SSE) |

**Identity and access, provided by apikit** ([section](#identity-and-access-endpoints-apikit-provided): [login](#login), [user](#user), [keys](#keys), [tokens](#tokens), [orgs](#orgs), [administration](#administration))

| Route | Purpose |
|-------|---------|
| [`GET /api/v1/auth/providers`](#get-apiv1authproviders) | List OAuth providers |
| [`POST /api/v1/auth/callback`](#post-apiv1authcallback) | Log in: exchange an OAuth code for an API key |
| [`GET /api/v1/user`](#get-apiv1user) · [`PATCH /api/v1/user`](#patch-apiv1user) | The caller's profile |
| [`GET /api/v1/user/keys`](#get-apiv1userkeys) · [`POST .../:key_id/refresh`](#post-apiv1userkeyskey_idrefresh) · [`DELETE .../:key_id`](#delete-apiv1userkeyskey_id) | The caller's API keys |
| [`GET /api/v1/user/tokens`](#get-apiv1usertokens) · [`POST`](#post-apiv1usertokens) · [`GET .../:token_id`](#get-apiv1usertokenstoken_id) · [`DELETE .../:token_id`](#delete-apiv1usertokenstoken_id) · [`.../:token_id/permissions`](#put--patch--delete-apiv1usertokenstoken_idpermissions) | The caller's PATs |
| [`GET /api/v1/user/orgs`](#get-apiv1userorgs) · [`GET /api/v1/orgs/:id`](#get-apiv1orgsid) | Organizations |
| [`/api/v1/users/...`](#users) · [`/api/v1/orgs/...`](#organizations) | [Administration](#administration) |

**Server** ([section](#server-endpoints))

| Route | Purpose |
|-------|---------|
| [`GET /healthz`](#get-healthz) · [`GET /readyz`](#get-readyz) | Liveness and readiness probes |
| [`GET /version`](#get-version) | Build information |
| [`GET /metrics`](#get-metrics) | Prometheus metrics |

---

## Conventions

- **Base URL.** Every REST route lives under the API mount point, `/api/v1` by
  default (`server.mount_point` in `config.toml`). Only these routes are served
  outside it, at the server root: `GET /healthz`, `GET /readyz`, `GET /version`,
  `GET /metrics` (see [Server Endpoints](#server-endpoints)), the git server
  under `/git/...` (see [Git Server Endpoints](#git-server-endpoints)) and the
  forge webhook ingress `POST /hooks/forge/:slug` (see
  [The webhook ingress](#the-webhook-ingress)).
- **Path parameters.** `:slug` after `/workspaces/` is a workspace slug; after
  `/orgs/` on the secrets and variables routes it is an organization slug. The
  apikit organization routes (`/orgs/:id`) take the organization's **id**, so a
  client cannot assume one identifier works for both.
- **Bodies** are JSON. A request body larger than the server's `max_body_size`
  is refused with `413` before any route sees it.
- **Timestamps** are RFC 3339; the hub renders them in UTC. Timestamps a client
  sends may carry an offset and are normalised to UTC.
- **Pagination.** The session, usage and audit lists page with an opaque
  `cursor`: a response carries `next_cursor` (null on the last page) and
  `has_more`; pass `next_cursor` back as `cursor` for the next page. Page-size
  defaults and caps are given per route.
- **Every entry** below has the same parts: what the route does, **Access**
  (the credential and scope it needs and the workspace access level),
  **Request**, **Response** and **Errors**. An entry's error table lists only
  the answers particular to that route; every route can also give the
  [common answers](#common-answers).

---

## Authentication

Every route except the public ones (`/healthz`, `/readyz`, `/version`,
`/metrics`, `GET /api/v1/auth/providers`, `POST /api/v1/auth/callback` and the
webhook ingress) needs a credential in the `Authorization: Bearer <credential>`
header:

- **API key.** Belongs to a user and acts with that user's full rights. It is
  created only by the OAuth login (`POST /api/v1/auth/callback`). An API key
  whose user has the admin role may use the
  [administration routes](#administration), but towards workspaces and the git
  server it is an ordinary user.
- **Personal access token (PAT).** Belongs to a user and is limited to the
  [permission scopes](#permission-scopes) it was granted. A PAT may also be
  [bound to one workspace](#workspace-bound-tokens).
- **Admin token.** A deployment credential with access to every workspace and
  user. It has no user identity, so it cannot own anything: it cannot create a
  workspace, mint a bound token or use the `/api/v1/user/secrets` and
  `/api/v1/user/vars` routes.

API keys and admin tokens hold every hub scope implicitly; only PATs are
checked against scopes. A request without a valid credential gets `401`.
Blocked users are refused by the API and by the git server.

Unless an entry says otherwise, a route accepts an API key, an admin token, or a
PAT with the scope the entry names.

---

## Permission Scopes

Every hub resource has one `read` and one `write` scope. The scope a route
needs is given in its entry.

| Scope | Grants |
|-------|--------|
| `workspaces:read` | List and read workspaces, the [rerere list](#get-apiv1workspacesslugrerere) and the [patch-status dashboard](#get-apiv1workspacesslugpatch-status) |
| `workspaces:write` | Create, update, archive, reactivate, sync, reclone and delete workspaces; push a branch to the forge; forget rerere resolutions |
| `patches:read` | List and read a workspace's patches |
| `patches:write` | Add, update, remove, restore, reorder and reset patches |
| `rebuilds:read` | Read rebuild jobs and the rebuild preview |
| `rebuilds:write` | Submit, cancel, requeue and roll back rebuilds |
| `merges:read` | List and read merge jobs |
| `merges:write` | Submit, cancel and requeue merge jobs; batch rebase |
| `git:read` | Clone and fetch through the [git server](#git-server-endpoints) |
| `git:write` | Push through the git server |
| `secrets:read` | List secret names (values are never returned) |
| `secrets:write` | Create, update and delete secrets |
| `vars:read` | List and read variables, including the resolved view |
| `vars:write` | Create, update and delete variables |
| `sessions:read` | Read agent sessions, their token usage and workspace cost |
| `sessions:write` | Open and complete agent sessions and report their usage |
| `audit:read` | Read per-run audit data, the unified audit query, transcripts and the event stream |
| `audit:write` | Ingest per-run audit data |
| `sandboxes:read` | List sandbox profiles; list and read sandbox leases |
| `sandboxes:write` | Issue and end sandbox leases |
| `forge:read` | Read a workspace's repository, issues and pull requests on its forge, how the hub reaches the forge, and the webhook delivery log |
| `forge:write` | Change issues, comments, labels and pull requests on the forge; set and remove the webhook secret |

The [identity and access routes](#identity-and-access-endpoints-apikit-provided)
use apikit's own scopes: `users:read`, `keys:read`, `keys:manage`,
`tokens:read`, `tokens:write`, `tokens:manage`, `orgs:read` and `orgs:write`
(which no route currently needs).
[permissions.md](permissions.md) describes every scope in detail.

### Implied Permissions

`write` implies `read`: a PAT with `workspaces:write` also lists and reads
workspaces, a PAT with `secrets:write` also lists secret names, and so on for
every hub resource. `read` never implies `write`.

The scope names of earlier releases (`workspaces:create`, `workspaces:delete`,
`workspaces:sync`, `secrets:list`, `secrets:manage`, `secrets:delete`,
`vars:manage`, `vars:delete`) are refused when a new PAT asks for them; PATs that
held them were converted to the matching `read` or `write` scope (see
[ADR 06](adr/06-collapse-scopes-to-read-and-write.md)).

---

## Workspace Access

Every route that names a workspace decides the caller's access the same way
(see [ADR 09](adr/09-decide-workspace-access-in-one-place.md)): an admin token
has `admin` access; the user behind an API key or PAT is `owner` of a
workspace they own and `member` when they belong to the workspace's
organization (whatever the organization's status or kind); everyone else has no
access. Membership is read on every request, so a user removed from the
organization loses access with the next request. Each route needs one of three
levels:

| Need | Admitted | A member gets | Anyone else gets |
|------|----------|---------------|------------------|
| read | owner, member, admin token | the owner's answer | `404 workspace not found` |
| write | owner, admin token | `403 org members have read-only access to this workspace`, `error_type: forbidden` | `404 workspace not found` |
| private | owner, admin token | `404 workspace not found`, as anyone else | `404 workspace not found` |

- **Read:** reading a workspace; the patch reads; the rebuild reads and the
  rebuild preview; the patch-status dashboard; the rerere list; the merge reads;
  the sandbox lease reads; every forge read and the webhook delivery log; the
  workspace variable reads (`GET .../vars`, `GET .../vars/resolved`). The
  workspace list returns the caller's own workspaces and those of the caller's
  organizations.
- **Write:** updating, archiving, reactivating, deleting, syncing and recloning
  a workspace; branch push; every patch write; submitting, cancelling,
  requeuing and rolling back rebuilds; forgetting rerere resolutions; merge
  submit, cancel, requeue and batch rebase; issuing and ending sandbox leases;
  every forge write and setting or removing the webhook secret; creating,
  updating and deleting workspace variables.
- **Private:** the workspace secret routes and the bound-token routes.

The 404 is the answer a slug that does not exist gets, so it never tells a
caller whether the workspace exists. The access check comes after the PAT scope
check, so a PAT without the scope gets the scope answer whatever its
membership; and before the route's status and precondition checks, so a
member's write to an archived workspace is `403`, not `400`. A failed access
lookup is `500`, never a grant. Every workspace object says what access the
caller has in `access` (see the [Workspace Response Schema](#workspace-response-schema)).

Some route families apply the same rule with their own answers:

- **Per-run audit reads and the transcript** need read access: the owner, a
  member and an admin token read them whatever the workspace's status; anyone
  else gets `404 workspace not found`.
- **Session and cost reads** need read access **and** an active workspace;
  everyone else, a member of an archived workspace included, gets
  `403 workspace access denied`.
- **The unified audit query and the event stream** return the events of every
  workspace the caller owns or shares an organization with, whatever its
  status. Hub events that belong to no workspace (user or credential changes)
  are visible to admin tokens only.
- **Opening a session** needs write access (a member gets `403 forbidden`).
  Completing a session and reporting its usage are for the session's creator
  only (`403 not session owner`), and every audit ingestion route is for the
  workspace's owner only (`403`, `error_type: workspace_access_denied`), so a
  member changes nothing there.
- **The git server**: a member clones and fetches like the owner; a member's
  push is refused (see [Git Access](#git-access)).

### Anti-Enumeration Policy

The workspace routes `GET /workspaces`, `GET`, `PATCH` and `DELETE
/workspaces/:slug`, and `POST .../archive` and `.../reactivate` answer a PAT
that lacks the scope with `404 workspace not found` rather than `403`, so that
they disclose nothing. Every other route, `POST /api/v1/workspaces` included,
answers a missing scope with `403`.

---

## Workspace-Bound Tokens

A PAT can be bound to exactly one workspace, `S`, when it is minted through
[`POST /api/v1/workspaces/:slug/tokens`](#post-apiv1workspacesslugtokens). It
then reaches `S` under its scopes, with exactly the answers an unbound PAT of
the same user and scopes gets, and nothing else:

| Request by a token bound to `S` | Answer |
|---|---|
| Any route whose path contains `workspaces/:slug`, for a slug other than `S` | `404 workspace not found`, before any scope, existence or status check |
| The same routes for `S` | The route's usual answer |
| `GET /api/v1/workspaces` | At most `S`; the scope check and `include_archived` apply as usual |
| `POST /api/v1/sessions` with another `workspace_slug`, or replaying another workspace's session id | `404 workspace not found` |
| `GET /api/v1/sessions/:id`, `GET /api/v1/sessions/:id/usage` or `GET /api/v1/sessions?workspace_slug=` outside `S` | `403 workspace access denied`; `GET /api/v1/sessions` lists only `S`'s sessions |
| `POST /api/v1/sessions/:id/complete` or `POST /api/v1/sessions/:id/usage` for a session outside `S` | `403 not session owner` |
| `GET /api/v1/audit`, `GET /api/v1/events` | Only `S`'s events; `GET /api/v1/audit?workspace=<other>` is `404 workspace not found` |
| `GET /api/v1/sandbox-profiles` | As for an unbound PAT |
| Every other hub route: `POST /api/v1/workspaces`, `/api/v1/user/secrets`, `/api/v1/user/vars`, `/api/v1/orgs/:slug/secrets`, `/api/v1/orgs/:slug/vars`, `GET /api/v1/sandboxes`, a path under the mount point that matches no route, and any route added later | `403`, `error_type: token_bound`, message `this token is bound to workspace <S>` |
| Every apikit route (`/auth/...`, `/user`, `/user/keys`, `/user/tokens`, `/users`, `/orgs`) | `403`, `error_type: token_bound`, message `this token is bound to a workspace` (the workspace is not named, because the token is refused before it is authenticated) |
| The git server, for a repository other than `S`'s | pkt-line `404 repository not found` |

A token bound to an archived workspace still reaches it and gets the usual
`400`/`409` answers for an archived workspace. A bound token's `tokens:*` scopes
have no effect: it can neither mint nor list bound tokens.

Deleting the workspace revokes its live bound tokens and removes their bindings
in the same transaction, so such a token then gets `401` from the API and the
git server. At start and every hour after, the hub also revokes the live tokens
of bindings whose workspace no longer exists and removes bindings whose token
no longer exists; bindings of revoked or expired tokens of an existing
workspace are kept, so the [token list](#get-apiv1workspacesslugtokens) shows
them. If the hub cannot read a binding it answers `500 internal server error`
(a pkt-line 500 on the git server) and never lets the token through as unbound.
API keys, admin tokens and unbound PATs are not affected.

Every refusal of a bound token is recorded as a `hub.token.bound_refused` event
(at most one per token per minute; see [Hub Audit Events](#hub-audit-events)).

---

## Error Response Schema

Errors use the apikit envelope:

```json
{
  "error": {
    "code": 409,
    "message": "a rebuild job is already queued or running for this workspace",
    "error_type": "concurrent_rebuild"
  }
}
```

`code` repeats the HTTP status and `message` describes the error for a person.
`error_type` is a machine-readable class, present only where the
[error types](#error-types) table lists one; it is omitted otherwise.

### Common answers

These can come from any route of the kind named, and the entries below do not
repeat them:

| Status | When |
|--------|------|
| `401` | No credential, or one the hub does not accept (`authentication required`) |
| `403` | A PAT without the scope the route needs (no `error_type`). The [workspace CRUD routes](#anti-enumeration-policy) answer `404` instead |
| `403` `token_bound` | A [bound token](#workspace-bound-tokens) on a route it may not use |
| `404` | A route that names a workspace, when the workspace does not exist or the caller may not see it at the level the route needs (`workspace not found`; see [Workspace Access](#workspace-access)) |
| `403` `forbidden` | A route that needs write access, called by a member of the workspace's organization (`org members have read-only access to this workspace`) |
| `409` `workspace_busy` | A route that works on the hub's clone while another operation does (see [Workspace Lock](#workspace-lock)) |
| `413` | A request body larger than the server accepts |
| `500` | A database or other internal failure (`internal server error`) |

### Error types

| Error type | Status | Routes | Meaning |
|------------|--------|--------|---------|
| `forbidden` | 403 | every route that needs write access | The caller is a member of the workspace's organization, who may read but not change it |
| `token_bound` | 403 | every route a bound token may not use | See [Workspace-Bound Tokens](#workspace-bound-tokens) |
| `workspace_busy` | 409 | archive, sync, reclone, rebuild rollback, batch rebase, rerere forget, reset-to-origin, branch push, adding a patch | Another operation is working on the workspace's clone; retry later. See [Workspace Lock](#workspace-lock) |
| `workspace_mode_mismatch` | 400 | rebuild submit; every forge route with `?target=upstream` | The workspace is not in `carry_patch` mode (a forge route with `target=upstream` also needs a non-empty `upstream_url`) |
| `no_active_patches` | 400 | rebuild submit | No patch has status `active` or `conflict` |
| `concurrent_rebuild` | 409 | rebuild submit | A rebuild is already queued or running for the workspace |
| `duplicate_merge` | 409 | merge submit | A merge job for the same source and target branch is already queued or running |
| `origin_fetch_failed` | 502 | sync, adding a patch, reset-to-origin | The fetch from the fork (`origin`) failed |
| `missing_on_origin` | 409 | reset-to-origin | The patch branch does not exist on the fork |
| `ref_changed` | 409 | reset-to-origin | Another operation changed the patch branch while the reset ran; retry |
| `branch_not_found` | 404 | branch push | The name breaks git's branch-name rules, or the hub's clone has no such branch |
| `integration_branch` | 400 | branch push | The name is a `carry_patch` workspace's integration branch |
| `patch_branch` | 400 | branch push | The name is a registered patch of a `carry_patch` workspace |
| `protected_branch` | 400 | branch push | `force` on the workspace's own branch |
| `non_fast_forward` | 409 | branch push | Origin's branch has commits the hub's branch lacks |
| `sandboxes_disabled` | 404 | every sandbox route | The hub has no `[sandbox]` section, so sandboxes are off. Answered to every authenticated caller before any scope check |
| `sandbox_capacity` | 409 | lease issue | The deployment's `max_sandboxes` limit on live leases is reached |
| `secret_missing` | 422 | lease issue | A secret the profile requires exists in no searched scope |
| `sandbox_provider_error` | 502 | lease end | The provider could not destroy the sandbox of an `active` lease, or did not answer within 30 seconds; the lease is unchanged |
| `body_too_large` | 413 | forge writes; webhook ingress | A body-like field is longer than the forge accepts, or a delivery is over 1 MiB |
| `forge_api_url_invalid` | 422 | every forge route | `forge_api_url` (or `upstream_forge_api_url`) is not an absolute `https://` URL with a host and without userinfo, query or fragment |
| `forge_unknown` | 422 | every forge route | The forge cannot be told from the host or an API URL setting, or the remote URL names no repository |
| `forge_not_found` | 404 | every forge route; branch push | The forge answered 404 (on a branch push: origin has no such repository, or the credential cannot see it) |
| `forge_unauthenticated` | 502 | every forge route; branch push | The forge answered 401 |
| `forge_forbidden` | 502 | every forge route; branch push | The forge answered 403 that is not a rate limit |
| `forge_rate_limited` | 429 | every forge route | A rate limit the hub could not wait out; `Retry-After` in whole seconds when known |
| `forge_rejected` | 422 | every forge route; branch push | The forge answered another 4xx (on a branch push: origin refused the ref) |
| `forge_error` | 502 | every forge route; branch push | The forge answered 5xx, an unreadable response or a redirect to another host; on a branch push, any failure no other type describes |
| `forge_unreachable` | 502 | every forge route; branch push | The connection failed at DNS, TCP or TLS level |
| `forge_timeout` | 504 | every forge route; branch push | The deadline passed: 30 seconds, two minutes for a branch push |
| `workspace_archived` | 409 | every audit ingestion route | The workspace is archived |
| `workspace_access_denied` | 403 | every audit ingestion route | The caller does not own the workspace |
| `unknown_schema_version` | 422 | postmortem submit | `schema_version` is not `1` |
| `postmortem_not_found` | 404 | postmortem read | The run has no postmortem |

---

## Workspace Lock

Operations that use the hub's clone of a workspace take turns: sync, reclone,
archive, rebuilds, merge jobs, batch rebase, rebuild rollback, rerere forget,
reset-to-origin, branch push, and adding a patch whose branch has to be
created in the clone. A request that finds
another of them running answers at once with `409`,
`error_type: workspace_busy`, and can be retried; rebuild and merge jobs wait
their turn instead.

A carry-patch rebuild applies its patches apart from the clone, so it blocks
the others only at its start (while it fetches upstream) and at its end (while
it updates the integration branch); while it applies patches, sync, rollback,
rerere forget, batch rebase, merges and branch pushes proceed. Archive and
reclone delete the directory the rebuild works in, so they answer
`409 workspace_busy` for the whole rebuild.

---

## Workspace Response Schema

Every route that returns a workspace returns this object:

```json
{
  "slug": "my-project",
  "git_url": "https://github.com/user/repo.git",
  "hub_url": "http://localhost:8080/git/org-slug/my-project.git",
  "branch": "main",
  "owner_id": "uuid-string",
  "org_id": "uuid-string-or-null",
  "org_slug": "org-slug-or-null",
  "access": "owner",
  "status": "ready",
  "display_name": "My Project",
  "description": "A description of the workspace",
  "head_sha": "abc123def456...",
  "clone_error": null,
  "upstream_head_sha": null,
  "last_sync_at": null,
  "sync_error": null,
  "workspace_mode": "standard",
  "upstream_url": null,
  "integration_branch": null,
  "settings": {"rebuild_strategy": "rebase", "sync_enabled": true, "...": "..."},
  "created_at": "2024-01-01T00:00:00Z",
  "updated_at": "2024-01-01T00:00:00Z"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `slug` | string | Globally unique, URL-safe identifier; immutable |
| `git_url` | string | HTTPS or SSH URL of the repository (the fork, for a `carry_patch` workspace); immutable |
| `hub_url` | string or null | Clone URL on the hub's [git server](#git-server-endpoints), e.g. `http://localhost:8080/git/org-slug/workspace-slug.git`. Null when `server.external_url` is not set or the workspace has no organization |
| `branch` | string or null | The workspace's branch; null means the repository's default branch; immutable |
| `owner_id` | string (UUID) | User who owns the workspace |
| `org_id` | string (UUID) or null | Organization of the workspace |
| `org_slug` | string or null | Slug of that organization; null when `org_id` is null or names no organization |
| `access` | string | The caller's access: `"admin"` (admin token), `"owner"` or `"member"` (member of the workspace's organization). It describes the caller, not a PAT's scopes; the create response always says `"owner"`. A member sees every other field as the owner does |
| `status` | string | `"pending"` (clone queued), `"cloning"`, `"ready"` (the clone is usable), `"failed"` (see `clone_error`) or `"archived"`. A sync in progress does not change it |
| `display_name` | string | Human-readable label; the slug when not set; never null or empty |
| `description` | string | Free-form text; empty string by default; never null |
| `head_sha` | string or null | 40-character SHA of the clone's `HEAD`; set when `status` is `"ready"` or `"archived"` |
| `clone_error` | string or null | Why the clone failed, when `status` is `"failed"` |
| `upstream_head_sha` | string or null | Head of the remote the last sync fetched (`origin` for a standard workspace, `upstream` for a `carry_patch` one); null until the first sync |
| `last_sync_at` | string or null | Time of the last successful sync; null until the first sync |
| `sync_error` | string or null | The last sync's error; null when it succeeded or none ran |
| `workspace_mode` | string | `"standard"` (one remote) or `"carry_patch"` (a fork with a patch list on top of an upstream); immutable |
| `upstream_url` | string or null | The upstream repository of a `carry_patch` workspace; null otherwise; immutable |
| `integration_branch` | string or null | The branch a `carry_patch` workspace rebuilds from the upstream base plus its patches; `"deploy"` by default; null for a standard workspace; immutable |
| `settings` | object | Every behaviour switch with its effective value; see [Workspace Settings Reference](#workspace-settings-reference) |
| `created_at` | string | When the workspace was created |
| `updated_at` | string | When it last changed |

---

## Workspace Endpoints

A workspace is the hub's record of one repository: a clone the hub keeps, the
git URL it came from and, for a `carry_patch` workspace, the upstream it
follows. These routes create, change and retire workspaces and keep their clone
up to date.

### POST /api/v1/workspaces

Create a workspace and start cloning its repository.

**Access:** API key, or PAT with `workspaces:write`. Admin tokens are refused
(`403`), because a workspace needs a real user as its owner.

**Request:**

```json
{
  "slug": "my-project",
  "git_url": "https://github.com/user/repo.git",
  "branch": "main",
  "org_id": "uuid-string",
  "display_name": "My Project",
  "description": "A description of the workspace",
  "workspace_mode": "standard",
  "upstream_url": "https://github.com/upstream/repo.git",
  "integration_branch": "deploy",
  "settings": {"patch_branch_source": "origin"},
  "git_pat": "ghp_xxxxxxxxxxxx",
  "git_username": "user",
  "git_password": "token-or-password"
}
```

| Field | Required | Type | Constraints |
|-------|----------|------|-------------|
| `slug` | yes | string | Globally unique. 3-64 characters; starts with a lowercase letter; ends with a lowercase letter or digit; only lowercase letters, digits and hyphens; no consecutive hyphens |
| `git_url` | yes | string | A valid HTTPS or SSH git URL |
| `branch` | no | string | Defaults to null (the repository's default branch) |
| `org_id` | no | string | Organization id or slug; the owner must be a member. When omitted or empty, the owner's personal organization is used |
| `display_name` | no | string | At most 128 characters; defaults to the slug |
| `description` | no | string | At most 1024 characters; defaults to `""` |
| `workspace_mode` | no | string | `"standard"` (default) or `"carry_patch"` |
| `upstream_url` | for `carry_patch` | string | Required for a `carry_patch` workspace, refused for a standard one; a valid HTTPS or SSH git URL |
| `integration_branch` | no | string | `carry_patch` only (refused for a standard workspace); defaults to `"deploy"` |
| `settings` | no | object | The [settings](#workspace-settings-reference) to set; keys not given take their defaults |
| `git_pat` | no | string | A token for a private repository. Excludes `git_username`/`git_password`; needs an HTTPS `git_url` |
| `git_username`, `git_password` | no | string | HTTP basic credentials; both or neither. Exclude `git_pat`; need an HTTPS `git_url` |

Credentials given here are checked against the repository before the workspace
is created, and stored as the workspace secrets `GIT_PAT`, or `GIT_USERNAME` and
`GIT_PASSWORD`, which every later clone, sync and push uses.

**Response:** `201 Created` with the [workspace](#workspace-response-schema),
`status` `"pending"`. Emits `hub.workspace.create`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `slug` or `git_url` missing or invalid; `display_name` or `description` too long; an invalid `workspace_mode`, `upstream_url` or `integration_branch` combination; an unknown setting or a value a setting cannot take |
| 400 | `git_pat` together with `git_username`/`git_password`; only one of `git_username` and `git_password`; an empty credential value; credentials with a non-HTTPS `git_url`; the credentials were refused by the repository |
| 400 | `org_id` names no organization; `org_id` omitted and the user has no personal organization |
| 403 | An admin token; a PAT without `workspaces:write`; the owner is not a member of `org_id` |
| 409 | A workspace with this `slug` exists |

### GET /api/v1/workspaces

List the workspaces the caller may see.

**Access:** `workspaces:read`. A PAT without it gets `404`
([anti-enumeration](#anti-enumeration-policy)).

**Request:** query parameter `include_archived` (boolean, default `false`)
includes archived workspaces.

**Response:** `200 OK` with an array of [workspaces](#workspace-response-schema),
newest first (by slug where `created_at` is equal). A user sees their own
workspaces (`access: "owner"`) and those of their organizations
(`access: "member"`); an admin token sees all (`access: "admin"`); a bound PAT
sees at most its own workspace.

**Errors:** only the [common answers](#common-answers).

### GET /api/v1/workspaces/:slug

Read one workspace.

**Access:** `workspaces:read` (a PAT without it gets `404`); read access.

**Response:** `200 OK` with the [workspace](#workspace-response-schema).

**Errors:** only the [common answers](#common-answers).

### PATCH /api/v1/workspaces/:slug

Change a workspace's mutable fields. Only the fields in the body change.

**Access:** `workspaces:write` (a PAT without it gets `404`); write access.

**Request:** at least one of:

```json
{
  "display_name": "New Display Name",
  "description": "Updated description",
  "org_id": "uuid-string",
  "settings": {"rebuild_strategy": "merge", "sync_enabled": false, "auto_rebuild_after_push": null}
}
```

| Field | Type | Constraints | `null` means |
|-------|------|-------------|--------------|
| `display_name` | string or null | At most 128 characters | Back to the slug |
| `description` | string or null | At most 1024 characters | Back to `""` |
| `org_id` | string or null | Organization id or slug; the owner must be a member | Remove the organization |
| `settings` | object or null | [Settings](#workspace-settings-reference) to change; keys not given are kept | A key given as `null` returns to its default; `"settings": null` resets every setting |

The immutable fields (`slug`, `git_url`, `branch`, `owner_id`,
`workspace_mode`, `upstream_url`, `integration_branch`) are refused, and so is
`sync_mode` (use the `sync_enabled` setting).

**Response:** `200 OK` with the updated [workspace](#workspace-response-schema).
Emits `hub.workspace.update`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | An empty body; the workspace is archived (reactivate it first); a field too long; an immutable field or `sync_mode`; an unknown setting or a value a setting cannot take; `org_id` names no organization |
| 403 | The owner is not a member of `org_id` |

### POST /api/v1/workspaces/:slug/archive

Archive a workspace. An archived workspace keeps its records and can be
reactivated; most writes refuse it.

**Access:** `workspaces:write` (a PAT without it gets `404`); write access.

**Behaviour:** for a `ready` workspace the hub first pushes every local branch
of its clone to `origin` with the workspace's stored credentials, because
branches pushed to the hub's git server exist only there; then it deletes the
clone. If that push fails, or there are no usable credentials, the workspace is
still archived but the clone is kept, so no commit is lost and reactivation
reuses it. A `pending` or `failed` workspace is archived without a push. The
workspace's active [agent sessions](#agent-session-endpoints) are ended with
status `terminated` (one `hub.session.force_closed` event each). Bound tokens
are left alone.

**Response:** `200 OK` with the [workspace](#workspace-response-schema),
`status` `"archived"`. Emits `hub.workspace.archive`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The workspace is already archived |
| 409 | The workspace is `cloning`; `workspace_busy` (another operation is running, or a rebuild is running at any stage) |

### POST /api/v1/workspaces/:slug/reactivate

Reactivate an archived workspace: its `status` becomes `"pending"` and the
clone is made again (or the kept clone reused).

**Access:** `workspaces:write` (a PAT without it gets `404`); write access.

**Response:** `200 OK` with the [workspace](#workspace-response-schema),
`status` `"pending"`. Emits `hub.workspace.reactivate`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 409 | The workspace is not archived |

### POST /api/v1/workspaces/:slug/sync

Bring the hub's clone up to date with the workspace's remote. What is fetched
depends on `workspace_mode`:

| `workspace_mode` | Fetches | With the credentials | Then |
|---|---|---|---|
| `standard` | `origin` (`git_url`) | `GIT_PAT`, or `GIT_USERNAME` + `GIT_PASSWORD` (none for a public repository) | The workspace's branch (`branch`, or the default branch) is fast-forwarded to origin's; `head_sha` and `upstream_head_sha` advance |
| `carry_patch` | `upstream` (`upstream_url`), not `origin` | `UPSTREAM_GIT_PAT`, or `UPSTREAM_GIT_USERNAME` + `UPSTREAM_GIT_PASSWORD`; else the origin credentials | Every upstream branch is fetched; `upstream_head_sha` advances, patches merged upstream are detected and a rebuild of the integration branch is queued. `head_sha` and the integration branch change only when that rebuild completes. See [Carry-Patch Sync Extension](#carry-patch-sync-extension) |

A `carry_patch` sync does not fetch the fork (`origin`) by default: patch
branches are read from the hub's clone, so they are pushed to the hub's
[git server](#git-server-endpoints), which also queues a rebuild when a
registered patch branch is pushed. With the setting `patch_branch_source` set
to `"origin"` the sync also fetches the fork and brings every registered patch
branch to the fork's tip. The rebuilt integration branch is pushed back to
`origin` only when the setting `rebuild_push_integration_branch` is `true`.

**Access:** `workspaces:write` (a PAT without it gets `403`); write access.

**Request:** query parameter `reset_to_upstream` (boolean, default `false`,
standard workspaces only): move the workspace's branch to origin's head whatever
the ancestry, to recover after a force-push. Ignored for `carry_patch`
workspaces. (For a standard workspace, "upstream" in `reset_to_upstream` and
`upstream_head_sha` means `origin`.)

**Preconditions:** `status` is `"ready"`, the `sync_enabled` setting is `true`,
and no other operation is using the clone ([Workspace Lock](#workspace-lock)).

**Outcomes of a standard sync:**

| Outcome | Description | Status |
|---------|-------------|--------|
| Up to date | Origin's head equals the local head; `last_sync_at` is updated | 200 |
| Fast-forward | Origin's head descends from the local head; `head_sha` advances to it | 200 |
| Reset | `reset_to_upstream=true`: the local branch is set to origin's head; `head_sha` and `upstream_head_sha` follow | 200 |
| Diverged | Origin's head does not descend from the local head (a force-push); `sync_error` is set and says to use `reset_to_upstream` | 409 |
| Fetch failure | Network, authentication or repository error; `sync_error` is set | 502 |
| Cancelled | The request was cancelled mid-sync (timeout or client disconnect); `sync_error` is set | 504 |

A failed standard sync records its error in `sync_error`, which the next
successful sync clears. A sync in progress is not a stored state: a second
sync meanwhile gets `409 workspace_busy`.

**Response:** `200 OK` with the [workspace](#workspace-response-schema): updated
`head_sha`, `upstream_head_sha` and `last_sync_at` and a null `sync_error`. A
`carry_patch` sync adds [its own fields](#carry-patch-sync-extension). A
standard sync emits `hub.workspace.sync` with `result` (`up_to_date`,
`fast_forward` or `reset_to_upstream`).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The workspace is not ready (not cloned, clone failed, or archived); `sync_enabled` is `false` |
| 403 | A PAT without `workspaces:write` |
| 409 | `workspace_busy` (a sync or another operation is running; a rebuild blocks only at its start and end); origin's history has diverged |
| 500 | `carry_patch` with `patch_branch_source=origin`: refreshing a patch branch failed (see [below](#carry-patch-sync-extension)) |
| 502 | The fetch failed; the credentials could not be resolved; `carry_patch` with `patch_branch_source=origin`: `failed to resolve origin credentials`, or `origin fetch failed` with `error_type: origin_fetch_failed` |
| 504 | The request was cancelled mid-sync |

#### Carry-Patch Sync Extension

On a `carry_patch` workspace the sync fetches `upstream` instead of
fast-forwarding a branch, and then works on the patch list. It differs from the
standard sync in these ways:

- `sync_error` is not used: a failure is reported by the status alone. When the
  credentials cannot be resolved or a fetch fails the answer is `502` and no
  workspace or patch state has changed (except that upstream tracking refs
  already fetched stay fetched).
- `head_sha` and the integration branch are changed by the queued rebuild, not
  by the sync; follow it with
  [`GET .../rebuilds/:id`](#get-apiv1workspacesslugrebuildsid).
- `reset_to_upstream` is ignored, and an upstream force-push is not an error:
  it is reported in `force_push_detected`, and the rebuild applies the patches
  to the rewritten upstream.
- `last_sync_at` is updated by every completed sync, whether or not anything
  advanced.

**What the sync does**, in order:

1. With `patch_branch_source=origin`, it resolves the fork's credentials
   (`502 failed to resolve origin credentials`).
2. It fetches every upstream branch, and the upstream default branch as the
   upstream head, which is the rebuild's base (`502` on failure).
3. With `patch_branch_source=origin`, it fetches the fork, pruning branches the
   fork deleted (`502 origin fetch failed`, `error_type: origin_fetch_failed`).
4. If upstream advanced and the previous `upstream_head_sha` is not an ancestor
   of the new head, it sets `force_push_detected`.
5. With `patch_branch_source=origin`, it refreshes every patch branch whose
   patch is `active`, `conflict` or `disabled` (never the integration branch)
   from the fork, by the first rule that applies: the fork lacks the branch
   (`missing_on_origin`); the hub lacks it (`created`); both are at the same
   commit (`none`); the fork's tip descends from the hub's (`fast_forwarded`);
   otherwise the branches have diverged, and under the `patch_divergence_policy`
   `replace` the hub's branch is set to the fork's tip and its old tip saved
   under `refs/hub/replaced/<branch>` (`replaced`), while under `report` it is
   left alone and reported as `diverged`. A branch that another operation
   changes while the sync writes it is not overwritten. Each patch's
   `origin_sync_state`, `origin_sha` and `origin_synced_at` are recorded. With
   `patch_branch_source=hub` those three fields are cleared on every patch.
6. If upstream advanced, or step 5 moved the branch of an `active` or
   `conflict` patch, it looks for patches merged upstream, by the strategies of
   the `squash_merge_detection` setting: **ancestry** (the patch branch is
   contained in upstream), **content** (upstream has every commit of the patch
   by content, which finds squash merges) and **PR number** (when the patch has
   an `upstream_pr_url`, a recent upstream commit message ends with
   `(#<number>)`, GitHub's squash-merge title). Either the ancestry or a content
   signal suffices. Matched patches become `merged_upstream`, and their origin
   fields are cleared.
7. It writes `last_sync_at`, and `upstream_head_sha` when upstream advanced.
8. Unless `auto_rebuild_after_sync` is `false`, it queues a rebuild when
   upstream advanced, a branch of an `active` or `conflict` patch moved
   (`created`, `fast_forwarded` or `replaced`), or a patch became
   `merged_upstream`. When a rebuild is already queued or running, nothing more
   is queued (`rebuild_triggered: false`).
9. It records audit events: `hub.patch.sync` and one `hub.patch.replace` per
   replaced branch, both only with `patch_branch_source=origin` (see
   [Hub Audit Events](#hub-audit-events)).

**A refresh failure** in step 5 ends the sync with `500`: the outcomes already
reached are recorded, a rebuild is queued for branches already moved (unless
`auto_rebuild_after_sync` is `false`), their events are emitted, and neither
`upstream_head_sha` nor `last_sync_at` is written. The message tells the cases
apart:

| Message | Meaning |
|---------|---------|
| `failed to update patch branch <branch>` | Writing the branch failed, or another operation changed it meanwhile |
| `failed to compare patch branch <branch> with origin` | The ancestry check failed; nothing was written |
| `failed to reset working tree for patch branch <branch>` | The branch moved (it counts as moved), but the checked-out files could not follow it |

**Response:** the [workspace](#workspace-response-schema), read after the sync,
with these fields added:

```json
{
  "slug": "my-workspace",
  "workspace_mode": "carry_patch",
  "upstream_head_sha": "abc123def456...",
  "last_sync_at": "2024-06-15T10:30:00Z",

  "patches_merged": ["feature/already-merged"],
  "rebuild_triggered": true,
  "rebuild_job_id": "d3b07384-d113-4ec5-8a4e-a12345678901",
  "force_push_detected": false,
  "origin_fetched": true,
  "patches_synced": [
    {
      "branch_name": "feature/auth-headers",
      "action": "fast_forwarded",
      "state": "in_sync",
      "local_sha": "abc123...",
      "origin_sha": "abc123..."
    }
  ],
  "patches_diverged": []
}
```

(Abbreviated: every workspace field is present.)

| Field | Type | Description |
|-------|------|-------------|
| `patches_merged` | string[] | Branches of the patches this sync found merged upstream |
| `rebuild_triggered` | boolean | Whether this sync queued a rebuild |
| `rebuild_job_id` | string | The queued rebuild's id; absent (never `null`) when none was queued |
| `force_push_detected` | boolean | Upstream's history was rewritten since the last sync. Informational |
| `origin_fetched` | boolean | Whether the fork was fetched (`patch_branch_source=origin`) |
| `patches_synced` | array | With `patch_branch_source=origin` only (`[]` when there are no candidates): per branch, `branch_name`, `action` (`none`, `created`, `fast_forwarded`, `replaced`), `state` (`in_sync`, `diverged`, `missing_on_origin`), `local_sha` (absent when the hub had no branch), `origin_sha` (absent when the fork has none) and `replaced_sha` (only for `replaced`). Describes step 5 even for patches step 6 then marked merged |
| `patches_diverged` | string[] | With `patch_branch_source=origin` only: the branches whose state is `diverged`; always `[]` under `replace` |

### POST /api/v1/workspaces/:slug/reclone

Recover a broken clone: push the clone's branches to `origin`, delete the
clone and clone again. `status` goes back to `"pending"` and follows the new
clone.

**Access:** `workspaces:write` (a PAT without it gets `403`); write access.

**Behaviour:** the push uses the workspace's stored credentials; if it fails
(no credentials, a refusal, a network error) the reclone goes on regardless.
The clone is then deleted, `sync_error` and `upstream_head_sha` are cleared
and the clone starts again.

**Response:** `200 OK` with the [workspace](#workspace-response-schema):

```json
{
  "slug": "my-workspace",
  "status": "pending",
  "sync_error": null,
  "upstream_head_sha": null
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The workspace is archived (reactivate it instead) |
| 403 | A PAT without `workspaces:write` |
| 409 | A clone is already under way (`status` `"pending"` or `"cloning"`); `workspace_busy` (another operation is running, or a rebuild is running at any stage) |
| 500 | The clone could not be deleted, or the workspace could not be updated |

### POST /api/v1/workspaces/:slug/branches/:name/push

Push one branch of the hub's clone to the workspace's `origin` (`git_url`).
This is how a branch pushed to the hub's git server reaches the forge, for
example before a pull request is opened with
[`POST .../pulls`](#the-write-routes).

The push sends exactly `refs/heads/<name>` to `refs/heads/<name>`, with the
workspace's stored credential (`GIT_PAT`, else `GIT_USERNAME` and
`GIT_PASSWORD`, else none), and is fast-forward only unless `force` is given.

**Access:** `workspaces:write`; write access. A PAT without the scope gets
`403 PAT requires workspaces:write scope to push branches` before the workspace
is looked up.

**Request:**

| Parameter | Description |
|-----------|-------------|
| `:name` | The branch name as one path segment, each `/` escaped as `%2F` (`fix%2F42-nil-map` names `fix/42-nil-map`). The segment is decoded exactly once, so `a%2525b` names `a%25b`. A name with an unescaped `/` matches no route (`404`) |

Body (optional): `{"force": false}`. `force` (boolean, default `false`)
overwrites origin's branch when it has commits the hub's branch lacks; it is
refused on the workspace's own branch. An empty body, `{}` and unknown fields
push without force; a body that is not a JSON object, or a `force` that is not
a boolean, is `400`.

**Order of checks:**

1. Credential (`401`), then a PAT's scope (`403`).
2. The workspace, with write access (`404`, `403 forbidden`).
3. The body (`400`), then the name: one that breaks git's branch-name rules or
   starts with `-` is `404 branch_not_found` naming the rule, for example
   `branch_not_found: "-x" is not a valid branch name: ref name must not start with '-'`.
4. `status` must be `"ready"` (`400 workspace is not ready; push requires a ready workspace`).
5. Branches this route does not push, the same whether the workspace is busy or
   not: on a `carry_patch` workspace the integration branch is
   `400 integration_branch` (it reaches the fork through the
   `rebuild_push_integration_branch` setting), a registered patch that is not
   `deleted` is `400 patch_branch` (patches reach the fork through the
   `push_patches_to_origin` setting); with `force`, the workspace's `branch` is
   `400 protected_branch`.
6. The [workspace lock](#workspace-lock), without waiting: `409 workspace_busy`.
7. With `force` and no `branch` set, the branch the clone's `HEAD` points to is
   `400 protected_branch`; a branch the clone lacks is
   `404 branch_not_found: branch "<name>" does not exist in the workspace`.
8. The push, with a two-minute deadline.

The push runs to its end (or its deadline) even if the caller goes away, and its
outcome is recorded as if the caller were there; calling again shows where the
branch stands.

**Response:** `200 OK`

```json
{
  "branch": "fix/42-nil-map",
  "sha": "<40-hex tip of the hub's branch that was offered to origin>",
  "remote": "origin",
  "remote_url": "https://github.com/acme/widgets.git",
  "result": "pushed"
}
```

`result` is `up_to_date` when origin already had the commit and nothing was
sent (also with `force`), `forced` when `force` was given and origin accepted an
update, and `pushed` otherwise. `remote_url` is `git_url` without userinfo; an
scp-style address (`git@host:path`) comes back unchanged.

Every push attempt (a `200`, `409 non_fast_forward`, `404 forge_not_found`, a
`422`, a `502` or the `504`) records one `hub.branch.push` event; refusals and
`500`s record nothing. No answer carries the credential, the userinfo of
`git_url` or the underlying git error.

**Errors:** the message of every answer with one of this route's own error
types begins with the type and `: `, because `afc` shows only the message (for
example `non_fast_forward: origin's feat is at <sha>, ...`). The answers shared
with other routes keep their usual messages.

| Status | Condition |
|--------|-----------|
| 400 | Invalid body; not ready; `integration_branch`, `patch_branch`, `protected_branch` |
| 403 | A PAT without `workspaces:write` |
| 404 | `branch_not_found`; `forge_not_found` (origin has no such repository, or the credential cannot see it) |
| 409 | `workspace_busy`; `non_fast_forward`: origin's branch has commits the hub's lacks. The message names origin's commit when the hub could list origin's refs and ends with `push with force to overwrite it`, or, on the workspace's own branch, `sync the workspace to bring it up to date`; when `force` was given and origin still refused, it says origin refuses non-fast-forward updates also with force |
| 422 | `forge_rejected`: origin refused the ref (a protected branch, a declining hook); the message carries origin's reason, scrubbed |
| 500 | A database or secrets store failure; the clone cannot be opened or has no `origin`; `HEAD` unreadable for a forced push without `branch` |
| 502 | `forge_unauthenticated` (the message says that the workspace has no stored credential, or which kind origin rejected: `GIT_PAT` or `GIT_USERNAME/GIT_PASSWORD`), `forge_forbidden`, `forge_unreachable`, `forge_error` |
| 504 | `forge_timeout`: the push did not end within two minutes |

### DELETE /api/v1/workspaces/:slug

Delete an archived workspace for good.

**Access:** `workspaces:write` (a PAT without it gets `404`); write access.

**Behaviour:** in the same transaction, every live token
[bound](#workspace-bound-tokens) to the workspace is revoked and every binding
removed, so those tokens get `401` from then on. The workspace's active agent
sessions are ended with status `terminated` (one `hub.session.force_closed`
event each).

**Response:** `204 No Content`. Emits `hub.workspace.delete`, whose
`revoked_tokens` lists the ids of the tokens this deletion revoked.

**Errors:**

| Status | Condition |
|--------|-----------|
| 409 | The workspace is not archived (archive it first) |
| 500 | `failed to delete workspace`; nothing was deleted |

---

## Workspace Settings Reference

A workspace's settings are its behaviour switches: how a `carry_patch`
workspace syncs, rebuilds and pushes, and where its forge's API is. They are
given in the `settings` object of
[`POST /api/v1/workspaces`](#post-apiv1workspaces), changed with the `settings`
object of [`PATCH /api/v1/workspaces/:slug`](#patch-apiv1workspacesslug) (a key
given takes the value, a key given as `null` returns to its default, a key not
given is kept), validated on every write (a `400` names the key and its
choices) and returned in full, with the effective value of every key, in every
workspace object. See [ADR 07](adr/07-make-the-behaviour-switches-workspace-settings.md).

| Setting | Type | Default | Description |
|---------|------|---------|-------------|
| `rebuild_strategy` | `"rebase"`, `"merge"` | `"rebase"` | How a rebuild applies patches. A rebuild's `strategy` field overrides it |
| `rebuild_fail_mode` | `"fail_fast"`, `"continue"` | `"fail_fast"` | Whether a rebuild stops at the first conflict or skips the conflicting patch and goes on. A rebuild's `fail_mode` field overrides it |
| `auto_rebuild_after_sync` | boolean | `true` | A `carry_patch` sync that finds upstream advanced, a patch branch moved (created, fast-forwarded or replaced for an `active` or `conflict` patch) or a patch merged queues a rebuild |
| `auto_rebuild_after_push` | boolean | `true` | A git push to a registered patch branch queues a rebuild |
| `patch_branch_source` | `"hub"`, `"origin"` | `"hub"` | Where patch branches come from. `"hub"`: the hub's clone. `"origin"`: the fork, fetched by every sync, which brings every registered patch branch to the fork's tip. Read by every sync |
| `patch_divergence_policy` | `"replace"`, `"report"` | `"replace"` | When the hub's and the fork's copy of a patch branch have diverged: `"replace"` sets the hub's copy to the fork's tip (the old tip is kept under `refs/hub/replaced/<branch>`); `"report"` leaves it and reports the divergence. Ignored with `patch_branch_source` `"hub"` |
| `push_patches_to_origin` | boolean | `false` | With `patch_branch_source` `"origin"`: a push to the hub on a registered patch branch is forwarded to the fork first (when `false`, it is refused). With `"hub"`: accepted pushes to registered patch branches are mirrored (force-pushed) to the fork. See [Push control](#pre-receive-hook-push-control) |
| `rebuild_push_integration_branch` | boolean | `false` | Every successful rebuild force-pushes the integration branch to `origin` |
| `squash_merge_detection` | `"ancestry_only"`, `"content_based"`, `"both"` | `"both"` | How a `carry_patch` sync finds patches merged upstream: `"ancestry_only"` by ancestry; `"content_based"` by content and PR number; `"both"` by all of them (see the [sync](#carry-patch-sync-extension)) |
| `forge_api_url` | string | `""` | The API URL of the forge behind `git_url`, for the [forge routes](#forge-endpoints): an absolute `https://` URL with a host and without userinfo, query or fragment, or empty to derive it from the host (`api.github.com` for `github.com`, `<host>/api/v3` or `<host>/api/v4` otherwise). A value that contains exactly one of `github` and `gitlab` also says which forge it is; a GitLab value gets `/api/v4` appended when it does not end with it |
| `upstream_forge_api_url` | string | `""` | The same for the `upstream_url` of a `carry_patch` workspace (`?target=upstream`). It does not fall back to `forge_api_url` |
| `sync_enabled` | boolean | `true` | Whether [`POST .../sync`](#post-apiv1workspacesslugsync) may run; `false` refuses every sync with `400` |

Workspace variables do not change hub behaviour: a variable that happens to be
named like a setting (`PATCH_BRANCH_SOURCE`, ...) is only a variable.

---

## Workspace-Bound Token Endpoints

These two routes mint and list PATs [bound to one workspace](#workspace-bound-tokens).

### POST /api/v1/workspaces/:slug/tokens

Mint a PAT bound to the workspace. The request is handled exactly as
[`POST /api/v1/user/tokens`](#post-apiv1usertokens) (the same body, validation,
expiry and privilege-escalation rules), and the new token is then bound.

**Access:** API key, or PAT with `tokens:manage` (checked as for
`POST /api/v1/user/tokens`); private access. Only the workspace's owner may
mint: admin tokens are refused, and a member of the workspace's organization
gets `404`, as a stranger does. A bound token cannot mint.

**Request:**

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `name` | yes | string | Token name |
| `permissions` | yes | string[] | Scopes of the token; a PAT caller may grant only scopes it holds |
| `expires` | no | integer | Days until expiry: 0 (never), 30, 60 or 90 |

**Response:** `201 Created`, `Cache-Control: no-store`. This is the only
response that carries the token:

```json
{
  "token_id": "k3x9q2mz",
  "token": "af_pat_k3x9q2mz_...",
  "name": "agent-fox service",
  "permissions": ["workspaces:read", "patches:read"],
  "workspace": "my-ws",
  "expires_at": "2027-01-19T10:00:00Z",
  "created_at": "2026-10-21T10:00:00Z"
}
```

Emits `hub.token.bind`. The hub never logs the token or the request's
`Authorization` header.

**Errors**, in the order they are checked:

| Status | Condition |
|--------|-----------|
| 401 | `authentication required` |
| 403 | `token_bound`: the caller is a bound PAT (one bound to another workspace gets `404 workspace not found`) |
| 403 | `admin tokens cannot mint bound tokens; a real user is required as the token's owner` |
| 404 | `workspace not found`: the workspace does not exist, or the caller is not its owner |
| 400 | `workspace is archived` |
| any | The answer of the token creation itself, unchanged (a bad body, an unknown scope, a missing `tokens:manage`, a PAT asking for scopes it does not hold, a bad `expires`); nothing is bound |
| 404 | `workspace not found`: the workspace was deleted while the token was created; the token is revoked |
| 500 | `failed to bind the token to the workspace`; the token is revoked |

### GET /api/v1/workspaces/:slug/tokens

List the tokens bound to the workspace, revoked and expired ones included,
ordered by creation time and then `token_id`. Works on archived workspaces.
Never carries a token or its hash.

**Access:** API key, admin token, or PAT with `tokens:read` or `tokens:manage`;
private access (a member of the workspace's organization gets `404`).

**Response:** `200 OK` with an array, `[]` when there is nothing to list:

```json
[
  {
    "token_id": "k3x9q2mz",
    "name": "agent-fox service",
    "permissions": ["workspaces:read", "patches:read"],
    "workspace": "my-ws",
    "expires_at": "2027-01-19T10:00:00Z",
    "revoked_at": null,
    "created_at": "2026-10-21T10:00:00Z"
  }
]
```

`expires_at` and `revoked_at` are `null` when unset. A binding whose token no
longer exists is left out.

**Errors:**

| Status | Condition |
|--------|-----------|
| 403 | `token_bound`: the caller is a bound PAT |
| 403 | `PAT requires tokens:read scope` |
| 404 | `workspace not found`: the workspace does not exist, or the caller is neither its owner nor an admin token |

---

## Agent Session Endpoints

An agent session records one agent run's use of a workspace: who opened it,
with which credential, its outcome and the tokens it consumed, for cost
reporting.

### POST /api/v1/sessions

Open an agent session.

**Access:** `sessions:write`; write access to `workspace_slug`, whatever the
workspace's status.

**Request:**

```json
{
  "workspace_slug": "my-workspace",
  "run_id": "run-1",
  "node_id": "node-1",
  "archetype": "coder",
  "model": "claude-3-5-sonnet",
  "metadata": {"key": "value"}
}
```

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `id` | no | string | Client-chosen UUID; generated when omitted |
| `workspace_slug` | yes | string | The workspace |
| `run_id` | no | string | Run the session belongs to |
| `node_id` | no | string | Task graph node |
| `archetype` | no | string | Agent archetype label |
| `model` | no | string | Model name |
| `metadata` | no | object | Arbitrary JSON |

**Response:** `201 Created` with the session; `200 OK` with the existing
session when `id` already exists and the caller may read its workspace.

```json
{
  "id": "uuid",
  "workspace_slug": "my-workspace",
  "credential_id": "cred-abc",
  "credential_type": "api_key",
  "status": "active",
  "started_at": "2026-09-01T00:00:00Z"
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `workspace_slug` missing or empty |
| 404 | `workspace not found`: the workspace does not exist or the caller may not see it, or a replayed `id` belongs to a workspace the caller may not read |

### POST /api/v1/sessions/:id/complete

Close an active session with a terminal status. Completing a session that is
already terminal returns it unchanged.

**Access:** `sessions:write`; the session's creator or an admin token.

**Request:**

```json
{
  "status": "completed",
  "error_message": "",
  "duration_ms": 3000,
  "cache_creation_input_tokens": 100
}
```

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `status` | yes | string | `completed`, `failed` or `timeout` |
| `error_message` | no | string | Error details, for `failed` |
| `duration_ms` | no | integer | Session duration in milliseconds |
| `cache_creation_input_tokens` | no | integer | Cache creation tokens |

**Response:** `200 OK` with the updated session.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `status` invalid, or `terminated` (set only by the hub) |
| 403 | `not session owner` |
| 404 | The session does not exist |
| 409 | Another close of the session is under way |

### POST /api/v1/sessions/:id/usage

Report token usage of an active session.

**Access:** `sessions:write`; the session's creator or an admin token.

**Request:**

```json
{
  "model": "claude-3-5-sonnet",
  "input_tokens": 200,
  "output_tokens": 80,
  "cache_read_tokens": 10
}
```

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `id` | no | string | Client-chosen UUID; generated when omitted |
| `model` | yes | string | Model name |
| `input_tokens` | no | integer | Input tokens, at least 0 |
| `output_tokens` | no | integer | Output tokens, at least 0 |
| `cache_read_tokens` | no | integer | Cache read tokens, at least 0 |

**Response:** `201 Created` with the usage record.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `model` missing or empty |
| 403 | `not session owner` |
| 404 | The session does not exist |
| 409 | The session is not active |
| 422 | A token count is negative |

### GET /api/v1/sessions

List sessions, newest first by default, with each session's `token_summary`.

**Access:** `sessions:read`. A user sees the sessions of the active workspaces
they own or share an organization with (an empty list when there are none); an
admin token sees all.

**Request:** query parameters:

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `workspace_slug` | string | | Only this workspace |
| `run_id` | string | | Only this run |
| `status` | string | | `active`, `completed`, `failed`, `timeout` or `terminated` |
| `credential_type` | string | | Only sessions opened with this credential type |
| `since` | string | | Sessions started after this time |
| `order` | string | `desc` | `asc` or `desc` |
| `limit` | integer | `50` | Page size, at most 500 |
| `cursor` | string | | [Pagination](#conventions) cursor |

**Response:** `200 OK`

```json
{
  "sessions": [...],
  "next_cursor": "string|null",
  "has_more": true
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 403 | `workspace access denied`: `workspace_slug` names a workspace the caller may not read, or an archived one |

### GET /api/v1/sessions/:id

Read one session with its `token_summary`.

**Access:** `sessions:read`; read access to an active workspace (an admin
token reads any).

**Response:** `200 OK` with the session.

**Errors:**

| Status | Condition |
|--------|-----------|
| 403 | `workspace access denied`: the session's workspace is archived, or the caller may not read it |
| 404 | The session does not exist |

### GET /api/v1/sessions/:id/usage

List a session's usage records, with totals over all of them.

**Access:** as for [`GET /api/v1/sessions/:id`](#get-apiv1sessionsid).

**Request:** query parameters `order` (`asc` or `desc`, default `desc`),
`limit` (default 100, at most 1000) and `cursor`.

**Response:** `200 OK`. `totals` covers every record of the session, whatever
the page.

```json
{
  "session_id": "sess-1",
  "records": [...],
  "totals": {
    "total_input_tokens": 500,
    "total_output_tokens": 200,
    "total_cache_read_tokens": 30,
    "models_used": ["claude-3-5-sonnet"]
  },
  "next_cursor": "string|null",
  "has_more": false
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | Invalid cursor |
| 403 | `workspace access denied`, as for the session |
| 404 | The session does not exist |

### GET /api/v1/workspaces/:slug/cost

Aggregate a workspace's token usage over a period.

**Access:** `sessions:read`; read access to an active workspace (an admin token
reads any). Any other caller, a member of an archived workspace included, gets
`403 workspace access denied`.

**Request:** query parameters:

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `group_by` | string | `day` | `day`, `session` or `model` |
| `since` | string | 24 hours ago | Start of the period |
| `until` | string | now | End of the period |

**Response:** `200 OK`

```json
{
  "workspace": "my-workspace",
  "period": {"since": "...", "until": "..."},
  "totals": {
    "input_tokens": 1000,
    "output_tokens": 500,
    "cache_read_tokens": 100,
    "sessions": 3
  },
  "breakdown": [
    {"date": "2026-09-01", "input_tokens": 1000, "output_tokens": 500, "cache_read_tokens": 100, "sessions": 3}
  ]
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `since` is not before `until`; an invalid `group_by` |
| 403 | `workspace access denied` |

---

## Patch Endpoints

A `carry_patch` workspace carries an ordered list of **patches**: branches the
hub applies, in position order, on top of the upstream base whenever it
[rebuilds](#rebuild-endpoints) the integration branch. These routes manage that
list. [carry_patch_workflow.md](carry_patch_workflow.md) walks through the
whole workflow.

Patch reads need read access to the workspace, patch writes write access (see
[Workspace Access](#workspace-access)).

### Patch Response Schema

```json
{
  "id": "uuid-string",
  "workspace_slug": "my-workspace",
  "branch_name": "feature/auth-headers",
  "position": 1,
  "status": "active",
  "conflict_files": ["src/config.go"],
  "upstream_pr_url": "https://github.com/upstream/repo/pull/42",
  "description": null,
  "deleted_at": null,
  "added_at": "2024-01-01T00:00:00Z",
  "updated_at": "2024-01-01T00:00:00Z",
  "origin_sync_state": "in_sync",
  "origin_sha": "abc123def456...",
  "origin_synced_at": "2024-06-15T10:30:00Z"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string (UUID) | The patch's id |
| `workspace_slug` | string | Its workspace |
| `branch_name` | string | Its branch; immutable |
| `position` | integer | 1-based application order; lower is applied first |
| `status` | string | `"active"`, `"merged_upstream"`, `"conflict"`, `"disabled"` or `"deleted"` |
| `conflict_files` | string[] | The conflicting files; present only while the patch has unresolved conflicts |
| `upstream_pr_url` | string or null | The upstream pull request of the patch |
| `description` | string or null | Free-form description |
| `deleted_at` | string or null | When the patch was removed by a rebuild after being merged upstream |
| `added_at` | string | When it was added |
| `updated_at` | string | When it last changed |
| `origin_sync_state` | string | How the hub's branch related to the fork's at the last sync: `"in_sync"`, `"diverged"` or `"missing_on_origin"`. Absent with `patch_branch_source` `"hub"` or before the first sync |
| `origin_sha` | string | The fork's tip of the branch at the last sync. Absent when the fork has no such branch, and with `patch_branch_source` `"hub"` |
| `origin_synced_at` | string | When the last sync compared it with the fork. Absent with `patch_branch_source` `"hub"` |

### POST /api/v1/workspaces/:slug/patches

Add one patch, or several at once, to a `carry_patch` workspace.

**Access:** `patches:write`; write access.

**Request:** one object, or a JSON array of them to add several in one
transaction:

```json
{
  "branch_name": "feature/auth-headers",
  "position": 2,
  "upstream_pr_url": "https://github.com/upstream/repo/pull/42",
  "description": "Add authentication headers to API",
  "skip_branch_check": false,
  "if_not_exists": false
}
```

| Field | Required | Type | Constraints |
|-------|----------|------|-------------|
| `branch_name` | yes | string | Not empty; not the integration branch; not already in the list |
| `position` | no | integer | 1-based; at least 1. Patches at that position and after move down by one. A value past the end appends; omitted, the patch is appended |
| `upstream_pr_url` | no | string | The upstream pull request |
| `description` | no | string | Free-form description |
| `skip_branch_check` | no | boolean | Do not check that the branch exists. Default `false` |
| `if_not_exists` | no | boolean | When the branch is already in the list, answer `200` with the existing patch instead of `409`. Default `false` |

A new patch gets status `"active"`.

**Branch check.** Unless `skip_branch_check` is `true`, the branch must exist,
as a full branch name (`refs/heads/<name>`: tags, bare SHAs and names such as
`origin/x` are not accepted). The hub looks for it in this order and stops at
the first that succeeds:

1. **`local`**: the hub's clone has the branch. Nothing is written.
2. **`origin_tracking`**: the clone has the fork's copy of the branch from an
   earlier fetch; a branch is created from it.
3. **`origin_fetch`**: only with `patch_branch_source` `"origin"`: the branch
   is fetched from the fork and created from it.

With `skip_branch_check`, none of this happens for that element. The setting
`patch_branch_source` is read once per request.

**Batches.** Every element is checked for syntax first, in array order
(`branch_name`, the integration branch, `position`), before any branch is
looked up: a batch whose first element names a missing branch and whose second
has `position: 0` answers `patch[1]: position must be >= 1`. Then every
element's branch is checked before anything is added; the first failure refuses
the whole batch (branches already created for earlier elements stay, and are
found as `local` next time). A `branch_name` given twice in one batch is `409`.

**Response:** `201 Created` with the patch, or with an array of patches for a
batch; `200 OK` with the existing patch for an `if_not_exists` hit. A single
add emits `hub.patch.create` with `branch_resolution` (`local`,
`origin_tracking`, `origin_fetch` or `skipped`); a batch emits none.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `branch_name` missing or empty, or the integration branch; `position` below 1; the workspace is `standard` or archived; the branch does not exist (in a batch, the message starts with `patch[i]: `) |
| 409 | The branch is already in the list (without `if_not_exists`); a branch given twice in a batch; `workspace_busy` (a branch has to be created while another operation uses the clone) |
| 502 | `origin_fetch_failed`: the fetch from the fork failed; `failed to resolve origin credentials` |

### GET /api/v1/workspaces/:slug/patches

List the workspace's patches in position order. Patches with status `"deleted"`
are left out; a standard workspace answers `[]`.

**Access:** `patches:read`; read access.

**Response:** `200 OK` with an array of [patches](#patch-response-schema).

**Errors:** only the [common answers](#common-answers).

### GET /api/v1/workspaces/:slug/patches/:id

Read one patch, a `"deleted"` one included.

**Access:** `patches:read`; read access.

**Response:** `200 OK` with the [patch](#patch-response-schema), plus
`replaced_sha` (the 40-character SHA) when the hub holds a backup of a tip it
replaced under `refs/hub/replaced/<branch>`. `replaced_sha` is absent when
there is no backup, when the workspace has no clone on disk (an archived
workspace), or when the lookup fails (which never fails the request). To
recover a replaced tip:

```
git fetch <hub_url> refs/hub/replaced/<branch>
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | `patch not found` |

### PATCH /api/v1/workspaces/:slug/patches/:id

Change a patch's position, status, description or upstream pull request. Only
the fields given change; `branch_name` is ignored if given.

**Access:** `patches:write`; write access.

**Request:**

```json
{
  "position": 1,
  "status": "disabled",
  "description": "Updated description",
  "upstream_pr_url": "https://github.com/upstream/repo/pull/99"
}
```

| Field | Type | Constraints |
|-------|------|-------------|
| `position` | integer | From 1 to the number of patches; the others shift to keep positions contiguous |
| `status` | string | `"active"`, `"merged_upstream"`, `"conflict"` or `"disabled"`. `"deleted"` cannot be set: remove a patch with `DELETE` |
| `description` | string | Free-form description |
| `upstream_pr_url` | string | The upstream pull request |

**Response:** `200 OK` with the updated [patch](#patch-response-schema).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | An invalid `status`, or `"deleted"`; `position` out of range |
| 404 | `patch not found` |
| 409 | The patch is `"deleted"`; restore it first |

### DELETE /api/v1/workspaces/:slug/patches/:id

Remove a patch from the list. The remaining patches close up so positions stay
contiguous. The hub's backup of a replaced tip (`refs/hub/replaced/<branch>`)
is removed too, on a best-effort basis that never changes the answer.

**Access:** `patches:write`; write access.

**Response:** `204 No Content`. Emits `hub.patch.delete`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | `patch not found` |
| 500 | The removal failed; nothing changed |

### POST /api/v1/workspaces/:slug/patches/:id/restore

Bring a `"deleted"` patch back as `"active"`, at the end of the list;
`deleted_at` is cleared.

**Access:** `patches:write`; write access.

**Response:** `200 OK` with the restored [patch](#patch-response-schema).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The workspace is archived or not `carry_patch`; the patch is not `"deleted"` |
| 404 | `patch not found` |

### POST /api/v1/workspaces/:slug/patches/:id/reset-to-origin

Set a patch branch to the fork's current tip, whatever the
`patch_divergence_policy`. The hub fetches the branch from the fork and moves
its own branch to the fork's tip. When that discards commits (the branches
diverged, or the fork's tip is behind), the old tip is saved under
`refs/hub/replaced/<branch>`, replacing any earlier backup.

**Access:** `patches:write`; write access.

**Request:** no body.

**Preconditions**, checked in order: the workspace is not archived and is
`carry_patch` (`400`); its `status` is `"ready"` (`409`); the patch exists
(`404`); its branch is not the integration branch (`400`); its status is
`active`, `conflict` or `disabled` (`409`).

**What happens**, by the first rule that applies:

| Case | Action | Backup |
|------|--------|--------|
| The hub has no such branch | `created` at the fork's tip | none |
| Same commit | `none`; nothing is written, still `200` | none |
| The fork's tip descends from the hub's | `fast_forwarded` | none |
| Anything else | `replaced` | the old tip, under `refs/hub/replaced/<branch>` |

With `patch_branch_source` `"origin"` the patch is recorded as `in_sync` at the
fork's tip. When the branch moved and the patch is `active` or `conflict`, a
rebuild is queued, unless `auto_rebuild_after_sync` is `false` (a `disabled`
patch queues none; an existing queued or running rebuild is not duplicated).
If the moved branch is checked out in the clone, the checked-out files follow;
if that fails after the branch moved, the request still succeeds and describes
the move.

**Response:** `200 OK` with the [patch](#patch-response-schema) as
[`GET`](#get-apiv1workspacesslugpatchesid) shows it (with `replaced_sha` when a
backup exists), plus:

| Field | Type | Description |
|-------|------|-------------|
| `replaced_sha` | string | The backup's SHA, when there is one |
| `rebuild_triggered` | boolean | Whether a rebuild was queued |
| `rebuild_job_id` | string | The queued rebuild's id; absent when none was queued |

Emits `hub.patch.reset`, and `hub.patch.replace` (with `trigger:
"reset_to_origin"`) for action `replaced`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The workspace is archived or not `carry_patch`; the branch is the integration branch |
| 404 | `patch not found` |
| 409 | The workspace is not ready; the patch is `merged_upstream` or `deleted`; `missing_on_origin`: the fork has no such branch; `ref_changed`: another operation changed the branch meanwhile, retry; `workspace_busy` |
| 500 | `failed to update patch branch`; `failed to fetch updated patch` (the branch moved and the event was emitted, but the patch could not be read back) |
| 502 | `origin_fetch_failed`; `failed to resolve origin credentials` |

### POST /api/v1/workspaces/:slug/patches/reorder

Set the order of the whole list at once: the first id gets position 1, the
second position 2, and so on.

**Access:** `patches:write`; write access.

**Request:**

```json
{
  "patch_ids": ["uuid-3", "uuid-1", "uuid-2"]
}
```

`patch_ids` must hold every patch of the workspace exactly once.

**Response:** `200 OK` with every [patch](#patch-response-schema) in the new
order.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | A missing or invalid body; `patch_ids` empty while the workspace has patches, missing an id, repeating one, or naming a patch of another workspace; the update failed |

---

## Rerere Management Endpoints

When a rebuild resolves a conflict, git's *rerere* (reuse recorded resolution)
records the resolution and replays it the next time the same conflict appears.
These routes list and forget a workspace's recorded resolutions.

### GET /api/v1/workspaces/:slug/rerere

List the recorded resolutions, newest first.

**Access:** `workspaces:read`; read access.

**Response:** `200 OK`; `{"resolutions": []}` when there are none.

```json
{
  "resolutions": [
    {
      "id": "3f2a9c0e7b1d4c5a8e6f0b2d1c3a4e5f6a7b8c9d",
      "path": "src/config.go",
      "recorded_at": "2024-06-15T10:30:00Z",
      "resolved": true
    },
    {
      "id": "9d8c7b6a5f4e3d2c1b0a9f8e7d6c5b4a3f2e1d0c",
      "path": null,
      "recorded_at": "2024-06-14T08:15:00Z",
      "resolved": false
    }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `resolutions[].id` | string | git's hash of the conflict; the id to pass to `DELETE` |
| `resolutions[].path` | string or null | The conflicting file. git knows it only while the conflict is in progress, so it is null for resolutions recorded by earlier rebuilds |
| `resolutions[].recorded_at` | string or null | When the entry was last written |
| `resolutions[].resolved` | boolean | `true` when a resolution is recorded and will be replayed; `false` when only the conflict has been seen |

**Errors:** only the [common answers](#common-answers).

### DELETE /api/v1/workspaces/:slug/rerere/\*pathspec

Forget a recorded resolution, by its `id` from the list (preferred) or by file
path. Forgetting by path works only while that file is in conflict.

**Access:** `workspaces:write`; write access.

**Request:** `*pathspec` is an `id` (40 or 64 hex characters) or a file path,
slashes included (`src/config.go`); a path must not start with `-`.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | An empty or invalid pathspec (checked before the workspace) |
| 404 | No entry with that id; no recorded resolution for that path |
| 409 | `workspace_busy` |

---

## Rebuild Endpoints

A **rebuild** makes a `carry_patch` workspace's integration branch afresh: the
upstream head plus every active patch, in position order. Rebuilds run as
queued jobs, one at a time per workspace; a sync, a push to a patch branch or a
patch reset queues one automatically, and these routes submit, follow, cancel,
requeue, roll back and preview them. Rebuild reads and the preview need read
access to the workspace, the other routes write access.

### Rebuild Job Response Schema

```json
{
  "id": "<uuid>",
  "status": "completed",
  "strategy": "rebase",
  "error": "",
  "created_at": "<rfc3339>",
  "completed_at": "<rfc3339>",
  "patch_results": [
    {
      "patch_id": "<uuid>",
      "branch_name": "feature/foo",
      "position": 1,
      "status": "success",
      "new_head_sha": "<sha>",
      "source_sha": "<sha>"
    }
  ],
  "integration_head_sha": "<sha>",
  "previous_integration_head_sha": "<sha>",
  "upstream_head_sha": "<sha>",
  "integration_branch_pushed": true
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string (UUID) | The job's id |
| `status` | string | `"queued"`, `"running"`, `"completed"`, `"failed"`, `"dead_letter"` or `"cancelled"` |
| `strategy` | string | `"rebase"` or `"merge"`; absent when empty |
| `error` | string | Why the job failed; absent when empty |
| `created_at` | string | When the job was queued |
| `completed_at` | string or null | When it ended; null until then |
| `patch_results` | array | Per-patch outcomes (see below): for a completed job, and for a running one as each patch finishes, so a client can poll progress |
| `integration_head_sha` | string | The integration branch's head after the rebuild; absent until the job completes |
| `previous_integration_head_sha` | string | Its head before the rebuild; absent for the first rebuild and until the job completes |
| `upstream_head_sha` | string | The upstream head the rebuild started from |
| `integration_branch_pushed` | boolean | `true` when the `rebuild_push_integration_branch` setting is on and the rebuilt branch was force-pushed to `origin`; absent otherwise |

Each `patch_results` entry:

| Field | Type | Description |
|-------|------|-------------|
| `patch_id` | string (UUID) | The patch |
| `branch_name` | string | Its branch |
| `position` | integer | Its position |
| `status` | string | `"success"`, `"conflict"` or `"skipped"` |
| `skipped_reason` | string | `"merged_upstream"`, `"disabled"`, `"deleted"` or `"branch_not_found"`; absent when not skipped |
| `new_head_sha` | string or null | The head after the patch was applied; null on conflict or skip |
| `conflict_files` | string[] | The conflicting files; absent without a conflict |
| `source_sha` | string | The patch branch's tip the rebuild applied; absent for a patch that was not attempted |

**How a rebuild works.** It starts from the upstream head it fetches at its
start and applies the patches apart from the hub's clone, whose checkout it
does not touch (see [ADR 02](adr/02-run-rebuilds-with-the-git-cli.md)). In
position order it skips patches that are `merged_upstream`, `disabled` or
`deleted` or whose branch does not exist, and applies each other patch by the
strategy: `rebase` copies each of the patch's own commits; `merge` merges the
branch with a merge commit. On a conflict it replays a recorded
[rerere](#rerere-management-endpoints) resolution; if a conflict remains,
`fail_fast` ends the job as failed, while `continue` marks the patch
`conflict`, leaves its changes out and goes on. At the end it sets the
integration branch to the result, marks the `merged_upstream` patches
`deleted` (with `deleted_at`), closes up the positions and, with
`rebuild_push_integration_branch`, force-pushes the integration branch to
`origin` (a failed push does not fail the job; it emits
`hub.rebuild.push_failed`). If a patch branch or the upstream head moved while
the job ran, the job still applies the tips it started with, and one follow-up
rebuild is queued afterwards (`submitted_by` `system:stale-snapshot`, event
`hub.rebuild.followup`). A rebuild blocks other operations only at its start
and end (see [Workspace Lock](#workspace-lock)).

### POST /api/v1/workspaces/:slug/rebuild

Queue a rebuild.

**Access:** `rebuilds:write`; write access.

**Request:** optional body:

```json
{
  "strategy": "rebase",
  "fail_mode": "fail_fast"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `strategy` | string | `"rebase"` or `"merge"`; overrides the `rebuild_strategy` setting |
| `fail_mode` | string | `"fail_fast"` or `"continue"`; overrides the `rebuild_fail_mode` setting |

Without a body the settings apply (`"rebase"` and `"fail_fast"` by default).

**Preconditions:** the workspace is `carry_patch` and `ready`, at least one
patch is `active` or `conflict`, and no rebuild is queued or running.

**Response:** `202 Accepted` with the queued job. Emits `hub.rebuild.enqueue`.

```json
{
  "id": "<uuid>",
  "type": "rebuild",
  "status": "queued",
  "key": "<workspace-slug>",
  "group_key": "<workspace-slug>:<integration-branch>",
  "payload": {
    "workspace_slug": "<slug>",
    "strategy": "rebase"
  }
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `workspace_mode_mismatch`: not `carry_patch`; the workspace is not ready; `no_active_patches`; an invalid `strategy` or `fail_mode` |
| 409 | `concurrent_rebuild`: a rebuild is already queued or running |

### GET /api/v1/workspaces/:slug/rebuilds

List the workspace's rebuild jobs, newest first.

**Access:** `rebuilds:read`; read access.

**Response:** `200 OK` with `{"jobs": [...]}`, each a
[rebuild job](#rebuild-job-response-schema).

**Errors:** only the [common answers](#common-answers).

### GET /api/v1/workspaces/:slug/rebuilds/:id

Read one rebuild job, with `patch_results` for a completed job and the progress
so far for a running one.

**Access:** `rebuilds:read`; read access.

**Response:** `200 OK` with the [rebuild job](#rebuild-job-response-schema).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such job in this workspace |

### DELETE /api/v1/workspaces/:slug/rebuilds/:id

Cancel a queued rebuild.

**Access:** `rebuilds:write`; write access.

**Response:** `200 OK` with `{"status": "cancelled"}`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such job in this workspace |
| 409 | The job is not `queued` |

### POST /api/v1/workspaces/:slug/rebuilds/:id/requeue

Queue a `dead_letter` rebuild again.

**Access:** `rebuilds:write`; write access.

**Response:** `200 OK` with the updated [rebuild job](#rebuild-job-response-schema).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such job in this workspace |
| 409 | The job is not `dead_letter`, or another rebuild of the workspace is queued or running |

### POST /api/v1/workspaces/:slug/rebuilds/:id/rollback

Set the integration branch back to where it was before a completed rebuild
(its `previous_integration_head_sha`).

**Access:** `rebuilds:write`; write access.

**Response:** `200 OK`

```json
{
  "rolled_back_to": "<sha>"
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such job in this workspace |
| 409 | The job has no previous head to return to (the first rebuild, or a job without a result); `workspace_busy` |
| 500 | The integration branch could not be reset |

### GET /api/v1/workspaces/:slug/rebuild-preview

Predict, without changing anything, which patches would apply cleanly. Every
patch that is not `disabled` or `merged_upstream` is tried in position order
on top of the upstream head and the patches before it that would succeed; a
predicted conflict does not stop the prediction of the next patches. A patch
whose branch does not exist is reported as `would_succeed`.

**Access:** `rebuilds:read`; read access.

**Response:** `200 OK`

```json
{
  "patch_results": [
    {
      "patch_id": "<uuid>",
      "branch_name": "feature/foo",
      "position": 1,
      "status": "would_succeed",
      "tree_sha": "<sha>",
      "conflict_files": []
    },
    {
      "patch_id": "<uuid>",
      "branch_name": "feature/bar",
      "position": 2,
      "status": "would_conflict",
      "conflict_files": ["src/main.go"]
    }
  ]
}
```

`status` is `"would_succeed"` or `"would_conflict"`; `tree_sha` is the
resulting tree on success (absent on conflict); `conflict_files` is `[]` on
success.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The workspace is not `carry_patch` |
| 500 | The upstream head could not be resolved, or the patches could not be read |

---

## Patch Status Dashboard Endpoint

### GET /api/v1/workspaces/:slug/patch-status

Summarise the health of a `carry_patch` workspace's patch stack in one answer:
the workspace's state, the latest rebuild, every patch with its latest rebuild
result, and counts.

**Access:** `workspaces:read`; read access.

**Response:** `200 OK`

```json
{
  "workspace_slug": "my-workspace",
  "workspace_mode": "carry_patch",
  "status": "ready",
  "head_sha": "fedcba987654...",
  "git_url": "https://github.com/example/repo.git",
  "upstream_url": "https://github.com/upstream/repo.git",
  "upstream_head_sha": "abc123def456...",
  "integration_branch": "integration",
  "integration_head_sha": "def456abc123...",
  "last_sync_at": "2024-06-15T10:30:00Z",
  "last_rebuild": {
    "id": "uuid-string",
    "status": "completed"
  },
  "patches": [
    {
      "id": "uuid-string",
      "branch_name": "feature/patch-a",
      "position": 1,
      "status": "active",
      "last_rebuild_result": "success"
    },
    {
      "id": "uuid-string",
      "branch_name": "feature/conflict",
      "position": 2,
      "status": "conflict",
      "last_rebuild_result": "conflict",
      "conflict_files": ["pkg/api.go"]
    }
  ],
  "summary": {
    "total_patches": 2,
    "active": 1,
    "merged_upstream": 0,
    "conflict": 1,
    "disabled": 0,
    "total_rerere_resolutions": 1,
    "patches_diverged": 0,
    "patches_missing_on_origin": 0
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `workspace_slug` | string | The workspace |
| `workspace_mode` | string | Always `"carry_patch"` |
| `status` | string | The workspace's `status` |
| `clone_error` | string | The clone error; absent when there is none |
| `sync_error` | string | The last sync's error; absent when it succeeded |
| `head_sha` | string | The workspace's head; `""` before the first clone |
| `git_url` | string | The fork's URL |
| `upstream_url` | string | The upstream's URL |
| `upstream_head_sha` | string | The upstream head at the last fetch |
| `integration_branch` | string | The integration branch |
| `integration_head_sha` | string | Its head after the latest rebuild; `""` when no rebuild has run |
| `last_sync_at` | string or null | When the workspace was last synced |
| `last_rebuild` | object or null | The latest rebuild job's `id` and `status` (`queued`, `running`, `completed`, `failed`); null when none has run |
| `patches` | array | Every patch that is not `"deleted"`, in position order |
| `patches[].id`, `.branch_name`, `.position`, `.status` | | As in the [patch](#patch-response-schema) |
| `patches[].last_rebuild_result` | string or null | The patch's result in the latest rebuild (`success`, `conflict`, `skipped`); null when none has run |
| `patches[].conflict_files` | string[] | Present only when `last_rebuild_result` is `conflict` |
| `patches[].origin_sync_state`, `.origin_sha`, `.origin_synced_at` | | As in the [patch](#patch-response-schema); absent with `patch_branch_source` `"hub"` |
| `summary.total_patches` | integer | The length of `patches`; equals `active + merged_upstream + conflict + disabled` |
| `summary.active`, `.merged_upstream`, `.conflict`, `.disabled` | integer | Patches per status |
| `summary.total_rerere_resolutions` | integer | [Recorded resolutions](#get-apiv1workspacesslugrerere) of the workspace; 0 when they cannot be read |
| `summary.patches_diverged`, `.patches_missing_on_origin` | integer | Patches whose `origin_sync_state` is `"diverged"` or `"missing_on_origin"`; always present, `0` with `patch_branch_source` `"hub"` |

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The workspace is not `carry_patch` |

---

## Merge Endpoints

A merge job brings an agent's branch into a target branch of the hub's clone:
the source is rebased onto the target and the target fast-forwarded to it.
Merge jobs run in the background and wait their turn for the workspace's clone
(see [Workspace Lock](#workspace-lock)). Merge reads need
read access to the workspace, the other routes write access.

### Merge Job Response Schema

```json
{
  "id": "uuid-string",
  "workspace_slug": "my-workspace",
  "target_branch": "main",
  "source_ref": "feature/agent-1",
  "status": "queued",
  "base_sha": null,
  "merged_sha": null,
  "conflict_files": [],
  "error": null,
  "retry_count": 0,
  "submitted_by": "alice",
  "created_at": "2024-01-01T00:00:00Z",
  "updated_at": "2024-01-01T00:00:00Z"
}
```

| Field | Type | Description |
|-------|------|-------------|
| `id` | string (UUID) | The job's id |
| `workspace_slug` | string | Its workspace |
| `target_branch` | string | The branch merged into |
| `source_ref` | string | The branch merged from |
| `status` | string | `"queued"`, `"running"`, `"completed"`, `"failed"`, `"dead_letter"` or `"cancelled"` |
| `base_sha` | string or null | The target's head before the merge; null until completed |
| `merged_sha` | string or null | The target's head after the merge; null until completed |
| `conflict_files` | string[] | The conflicting files; `[]` without a conflict |
| `error` | string or null | Why the job failed |
| `retry_count` | integer | How often the job was retried |
| `submitted_by` | string | The user who submitted it |
| `created_at`, `updated_at` | string | When it was queued and last changed |

A job emits `hub.merge.enqueue` when it is queued and `hub.merge.complete` or
`hub.merge.fail` when it ends.

### POST /api/v1/workspaces/:slug/merges

Queue a merge of `source_ref` into `target_branch`.

**Access:** `merges:write`; write access.

**Request:**

```json
{
  "target_branch": "main",
  "source_ref": "feature/agent-1"
}
```

Both are required, must follow git's branch-name rules (no leading `-`, no
`..`, no control characters, ...) and must exist in the workspace. The body is
checked before the workspace.

**Response:** `202 Accepted` with the queued [merge job](#merge-job-response-schema).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | A malformed body; a missing or invalid branch name; the workspace is not ready; a branch does not exist |
| 409 | `duplicate_merge`: a merge of the same source into the same target is queued or running |

### GET /api/v1/workspaces/:slug/merges

List the workspace's merge jobs (at most 200).

**Access:** `merges:read`; read access.

**Response:** `200 OK` with an array of [merge jobs](#merge-job-response-schema),
`[]` when there are none.

**Errors:** only the [common answers](#common-answers).

### GET /api/v1/workspaces/:slug/merges/:id

Read one merge job.

**Access:** `merges:read`; read access.

**Response:** `200 OK` with the [merge job](#merge-job-response-schema).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such job in this workspace |

### DELETE /api/v1/workspaces/:slug/merges/:id

Cancel a queued merge job.

**Access:** `merges:write`; write access.

**Response:** `200 OK` with `{"status": "cancelled"}`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such job in this workspace |
| 409 | The job is not `queued` (running, completed, failed or cancelled) |

### POST /api/v1/workspaces/:slug/merges/:id/requeue

Queue a `dead_letter` merge job again.

**Access:** `merges:write`; write access.

**Response:** `200 OK` with the updated [merge job](#merge-job-response-schema).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such job in this workspace |
| 409 | The job is not `dead_letter`, or a merge of the same source and target is queued or running |

### POST /api/v1/workspaces/:slug/rebase

Rebase several branches onto a target, one after the other, and answer with
each one's result. It runs while the request waits; a conflict on one branch
does not stop the others.

**Access:** `merges:write`; write access.

**Request:**

```json
{
  "target_ref": "main",
  "branches": ["feature/a", "feature/b", "feature/c"]
}
```

`target_ref` is required; `branches` must not be empty. Every name must follow
git's branch-name rules. The body is checked before the workspace.

**Response:** `200 OK`

```json
{
  "results": [
    {
      "branch": "feature/a",
      "status": "ok",
      "new_head": "aaaa000000000000000000000000000000000001"
    },
    {
      "branch": "feature/b",
      "status": "conflict",
      "conflict_files": ["src/main.go", "src/util.go"]
    }
  ]
}
```

`status` is `"ok"`, `"conflict"` or `"error"`; `new_head` (the branch's new
head) is present only for `ok`, `conflict_files` only for a conflict.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | A malformed body; `target_ref` missing; `branches` empty; an invalid name (`invalid branches[<i>]: ...`); the workspace is not ready |
| 409 | `workspace_busy` |
| 500 | The rebase failed |

---

## Git Server Endpoints

The hub serves every workspace's clone over git's smart HTTP protocol, for
clone, fetch and push, at:

```
<external_url>/git/<org-slug>/<workspace-slug>.git
```

The `.git` suffix is required; a URL without it is `404`. A workspace without
an organization has no URL. The workspace's `hub_url` field gives the URL.

### Git Server Authentication

HTTP Basic authentication: the username is ignored and the password is a hub
credential (API key, PAT or admin token). Git clients send it with, for
example, `git clone https://x:<credential>@hub.example.com/git/acme/widgets.git`
or a credential helper.

### Git Access

The git server decides access with the [workspace access](#workspace-access)
rule. A push (`POST .../git-receive-pack`, and `GET .../info/refs?service=git-receive-pack`)
needs write access; everything else, `info/refs` with a missing or invalid
`service` included, needs read access. Refusals are git pkt-line errors, which
git shows to the user:

| Caller | Clone and fetch | Push |
|--------|-----------------|------|
| The workspace's owner, an admin token | served | served |
| A member of the workspace's organization | served, as for the owner | `403 ERR org members have read-only access to this workspace` |
| Anyone else | `404 ERR repository not found` | `404 ERR repository not found` |

An API key whose user has the admin role is not an admin token. The checks run
in this order: the workspace and its organization must match the URL
(`404 repository not found`) and its clone must be ready
(`404 repository not available`); then access; then a PAT's scope, so a
member's push is refused with the `403` whatever the PAT's scopes. A failed
access lookup is a pkt-line `500 internal server error`. A PAT bound to another
workspace gets the `404`. A refused push changes nothing and records no event.

### Git Permission Scopes

| Scope | Allows |
|-------|--------|
| `git:read` | Clone and fetch |
| `git:write` | Push (implies `git:read`) |

API keys and admin tokens hold both implicitly, but access comes first: a
member's API key still cannot push.

### Pre-Receive Hook (Push Control)

On a `carry_patch` workspace the hub decides what happens to a pushed update of
a **registered patch branch**: a `refs/heads/<name>` whose `<name>` is a patch
that is not `deleted` and is not the integration branch. Every other update
(other branches, tags, standard workspaces) is unaffected. What happens depends
on the settings `patch_branch_source` and `push_patches_to_origin`:

| `patch_branch_source` | `push_patches_to_origin` | A pushed update of a patch branch |
|---|---|---|
| `origin` | `false` | **Refused**: `branch is synced from origin; push to <git_url> instead` (`<git_url>` without userinfo), or `branch is synced from origin; delete it on the fork instead` for a delete |
| `origin` | `true` | **Forwarded**: pushed to the fork first (not forced); only when the fork accepts it does the hub accept it. A failure is reported as `origin rejected push: <error>` or `failed to resolve origin credentials`. A delete is refused as above |
| `hub` | `true` | **Mirrored**: accepted, then force-pushed to the fork on a best-effort basis. A failed mirror emits `hub.patch.mirror_failed` and fails neither the push nor the rebuild |
| `hub` | `false` | Accepted |

A refused or failed update is reported for that ref only, as a `remote
rejected` line in `git push`'s output; the other refs of the same push are
processed on their own.

### Protected Namespace: `refs/hub/replaced/`

The hub keeps backups of replaced patch tips under `refs/hub/replaced/`. No
client push may create, update or delete a ref there, on any workspace and with
any credential: such an update is reported as `ng <ref> refs/hub/replaced/ is
maintained by the hub and cannot be pushed to` (the push itself reports
`unpack ok`, the other refs are processed as usual, and the ref keeps its old
value). Fetching them works normally: `git ls-remote` and
`git fetch <hub_url> refs/hub/replaced/<branch>`.

### After a push

Only the ref updates the hub accepted (reported `ok`) have effects:

1. The workspace's `head_sha` is refreshed from the clone's `HEAD` (after every
   push, even one whose every ref was refused), and the clone's checked-out
   files follow the new `HEAD`.
2. A `hub.git.push` event lists the accepted refs in `refs_updated`; a push
   with no accepted update records no event.
3. On a `carry_patch` workspace, an accepted update of a registered patch branch
   queues a rebuild, unless `auto_rebuild_after_push` is `false`. The push's
   answer does not wait for it.
4. With `patch_branch_source` `"hub"` and `push_patches_to_origin`, every
   accepted patch branch is then [mirrored](#pre-receive-hook-push-control) to
   the fork, whether or not a rebuild was queued.

### GET /git/:org/:slug.git/info/refs

Git's ref advertisement (smart HTTP discovery).

**Access:** HTTP Basic; read access, or write access for
`service=git-receive-pack`; a PAT needs `git:read` or `git:write` for the
service.

**Request:** query parameter `service`: `git-upload-pack` (fetch, clone) or
`git-receive-pack` (push).

**Response:** `200 OK`, `Content-Type: application/x-<service>-advertisement`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 401 | Missing or invalid credentials |
| 403 | A member asking for `git-receive-pack`; a missing or invalid `service` (`ERR invalid or missing service parameter`); a PAT without the git scope |
| 404 | The workspace does not exist, does not match the organization, has no ready clone, or the caller may not see it |

### POST /git/:org/:slug.git/git-upload-pack

Fetch and clone data transfer. The full negotiation is supported, so a fetch
into a repository that already has part of the history transfers only the
missing objects. The server speaks protocol v0 (a client asking for v2 falls
back to v0) and accepts gzip-encoded request bodies (`Content-Encoding: gzip`).

**Access:** HTTP Basic; read access; a PAT needs `git:read`.

**Response:** `200 OK`, `Content-Type: application/x-git-upload-pack-result`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 401 | Missing or invalid credentials |
| 403 | A PAT without `git:read` |
| 404 | As for [`info/refs`](#get-gitorgsluggitinforefs) |

### POST /git/:org/:slug.git/git-receive-pack

Push data transfer. Updates of patch branches on a `carry_patch` workspace are
subject to [push control](#pre-receive-hook-push-control), and accepted
updates have the effects listed under [After a push](#after-a-push).

**Access:** HTTP Basic; write access; a PAT needs `git:write`.

**Response:** `200 OK`, `Content-Type: application/x-git-receive-pack-result`,
with git's per-ref report.

**Errors:**

| Status | Condition |
|--------|-----------|
| 401 | Missing or invalid credentials |
| 403 | A member of the workspace's organization (whatever the PAT's scopes); a PAT without `git:write` |
| 404 | As for [`info/refs`](#get-gitorgsluggitinforefs) |

---

## Secrets Endpoints

Secrets are named values kept for a user, an organization or a workspace, for
example the git credentials of a workspace (`GIT_PAT`, `UPSTREAM_GIT_PAT`, ...),
its forge webhook secret (`FORGE_WEBHOOK_SECRET`) or the secrets a
[sandbox](#sandbox-lease-endpoints) receives. Their values are **never
returned**: responses carry names and times only.

**The three scopes:**

| Routes | Whose | Access |
|--------|-------|--------|
| `/api/v1/user/secrets` | The caller's own | Any user; admin tokens get `403` (they have no user) |
| `/api/v1/orgs/:slug/secrets` | An organization's, by slug | Members of the organization and admin tokens; anyone else gets `404` |
| `/api/v1/workspaces/:slug/secrets` | A workspace's | Private access: the owner and admin tokens; anyone else, a member of its organization included, gets `404` |

**Rules:**

| Rule | Constraint |
|------|------------|
| Key format | Letters, digits and underscores; does not start with a digit |
| Key length | At most 255 characters |
| Key matching | Case-insensitive, in lookups and in the uniqueness check |
| Value size | At most 256 KB (262,144 bytes) |
| Entries | At most 100 per scope (one user, one organization or one workspace) |

**A secret** in a response:

```json
{
  "key": "MY_SECRET",
  "created_at": "2024-01-01T00:00:00Z",
  "updated_at": "2024-01-01T00:00:00Z"
}
```

Every change emits `hub.secret.create`, `hub.secret.update` or
`hub.secret.delete` with the scope type and the key, never the value.

### POST /api/v1/user/secrets

Create one or more of the caller's secrets.

**Access:** `secrets:write`; a user (not an admin token).

**Request:**

```json
{
  "entries": [
    {"key": "MY_SECRET", "value": "secret-value"},
    {"key": "ANOTHER_SECRET", "value": "another-value"}
  ]
}
```

**Response:** `201 Created` with an array of the created [secrets](#secrets-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | No entries; an invalid key; a value too large; the scope would exceed 100 entries |
| 409 | A key exists already (ignoring case) |

### GET /api/v1/user/secrets

List the caller's secrets, sorted by key ignoring case.

**Access:** `secrets:read`; a user.

**Response:** `200 OK` with an array of [secrets](#secrets-endpoints).

**Errors:** only the [common answers](#common-answers).

### PATCH /api/v1/user/secrets/:key

Replace the value of one of the caller's secrets.

**Access:** `secrets:write`; a user.

**Request:** `{"value": "new-secret-value"}`; `:key` is matched ignoring case.

**Response:** `200 OK` with the [secret](#secrets-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `value` missing |
| 404 | No such key |

### DELETE /api/v1/user/secrets/:key

Delete one of the caller's secrets.

**Access:** `secrets:write`; a user.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such key |

### POST /api/v1/orgs/:slug/secrets

Create one or more secrets of an organization.

**Access:** `secrets:write`; a member of the organization or an admin token.

**Request:** as for [`POST /api/v1/user/secrets`](#post-apiv1usersecrets).

**Response:** `201 Created` with an array of the created [secrets](#secrets-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | As for the user scope |
| 404 | The organization does not exist, or the caller is not a member |
| 409 | A key exists already (ignoring case) |

### GET /api/v1/orgs/:slug/secrets

List an organization's secrets, sorted by key ignoring case.

**Access:** `secrets:read`; a member of the organization or an admin token.

**Response:** `200 OK` with an array of [secrets](#secrets-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | The organization does not exist, or the caller is not a member |

### PATCH /api/v1/orgs/:slug/secrets/:key

Replace the value of an organization's secret.

**Access:** `secrets:write`; a member of the organization or an admin token.

**Request:** `{"value": "..."}`.

**Response:** `200 OK` with the [secret](#secrets-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `value` missing |
| 404 | The organization does not exist, the caller is not a member, or no such key |

### DELETE /api/v1/orgs/:slug/secrets/:key

Delete an organization's secret.

**Access:** `secrets:write`; a member of the organization or an admin token.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | The organization does not exist, the caller is not a member, or no such key |

### POST /api/v1/workspaces/:slug/secrets

Create one or more secrets of a workspace.

**Access:** `secrets:write`; private access.

**Request:** as for [`POST /api/v1/user/secrets`](#post-apiv1usersecrets).

**Response:** `201 Created` with an array of the created [secrets](#secrets-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | As for the user scope |
| 409 | A key exists already (ignoring case) |

### GET /api/v1/workspaces/:slug/secrets

List a workspace's secrets, sorted by key ignoring case.

**Access:** `secrets:read`; private access.

**Response:** `200 OK` with an array of [secrets](#secrets-endpoints).

**Errors:** only the [common answers](#common-answers).

### PATCH /api/v1/workspaces/:slug/secrets/:key

Replace the value of a workspace's secret.

**Access:** `secrets:write`; private access.

**Request:** `{"value": "..."}`.

**Response:** `200 OK` with the [secret](#secrets-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `value` missing |
| 404 | No such key |

### DELETE /api/v1/workspaces/:slug/secrets/:key

Delete a workspace's secret.

**Access:** `secrets:write`; private access.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such key |

---

## Variables Endpoints

Variables are named values like [secrets](#secrets-endpoints), kept for a user,
an organization or a workspace, but their values **are returned**. They follow
the same key, size and count rules and the same scopes, except that workspace
variables need read access to read and write access to change (a member of the
workspace's organization reads them and gets `403 forbidden` on a change).
Variables do not change hub behaviour; the hub's switches are
[workspace settings](#workspace-settings-reference).

**A variable** in a response:

```json
{
  "key": "MY_VAR",
  "value": "my-value",
  "created_at": "2024-01-01T00:00:00Z",
  "updated_at": "2024-01-01T00:00:00Z"
}
```

Every change emits `hub.variable.create`, `hub.variable.update` or
`hub.variable.delete` with the scope type and the key.

### POST /api/v1/user/vars

Create one or more of the caller's variables.

**Access:** `vars:write`; a user (admin tokens get `403`).

**Request:**

```json
{
  "entries": [
    {"key": "MY_VAR", "value": "my-value"},
    {"key": "ANOTHER_VAR", "value": "another-value"}
  ]
}
```

**Response:** `201 Created` with an array of the created [variables](#variables-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | No entries; an invalid key; a value too large; the scope would exceed 100 entries |
| 409 | A key exists already (ignoring case) |

### GET /api/v1/user/vars

List the caller's variables, sorted by key ignoring case.

**Access:** `vars:read`; a user.

**Response:** `200 OK` with an array of [variables](#variables-endpoints).

**Errors:** only the [common answers](#common-answers).

### PATCH /api/v1/user/vars/:key

Replace the value of one of the caller's variables.

**Access:** `vars:write`; a user.

**Request:** `{"value": "new-value"}`; `:key` is matched ignoring case.

**Response:** `200 OK` with the [variable](#variables-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `value` missing |
| 404 | No such key |

### DELETE /api/v1/user/vars/:key

Delete one of the caller's variables.

**Access:** `vars:write`; a user.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such key |

### POST /api/v1/orgs/:slug/vars

Create one or more variables of an organization.

**Access:** `vars:write`; a member of the organization or an admin token.

**Request:** as for [`POST /api/v1/user/vars`](#post-apiv1uservars).

**Response:** `201 Created` with an array of the created [variables](#variables-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | As for the user scope |
| 404 | The organization does not exist, or the caller is not a member |
| 409 | A key exists already (ignoring case) |

### GET /api/v1/orgs/:slug/vars

List an organization's variables, sorted by key ignoring case.

**Access:** `vars:read`; a member of the organization or an admin token.

**Response:** `200 OK` with an array of [variables](#variables-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | The organization does not exist, or the caller is not a member |

### PATCH /api/v1/orgs/:slug/vars/:key

Replace the value of an organization's variable.

**Access:** `vars:write`; a member of the organization or an admin token.

**Request:** `{"value": "..."}`.

**Response:** `200 OK` with the [variable](#variables-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `value` missing |
| 404 | The organization does not exist, the caller is not a member, or no such key |

### DELETE /api/v1/orgs/:slug/vars/:key

Delete an organization's variable.

**Access:** `vars:write`; a member of the organization or an admin token.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | The organization does not exist, the caller is not a member, or no such key |

### POST /api/v1/workspaces/:slug/vars

Create one or more variables of a workspace.

**Access:** `vars:write`; write access.

**Request:** as for [`POST /api/v1/user/vars`](#post-apiv1uservars).

**Response:** `201 Created` with an array of the created [variables](#variables-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | As for the user scope |
| 409 | A key exists already (ignoring case) |

### GET /api/v1/workspaces/:slug/vars

List a workspace's variables, sorted by key ignoring case.

**Access:** `vars:read`; read access.

**Response:** `200 OK` with an array of [variables](#variables-endpoints).

**Errors:** only the [common answers](#common-answers).

### PATCH /api/v1/workspaces/:slug/vars/:key

Replace the value of a workspace's variable.

**Access:** `vars:write`; write access.

**Request:** `{"value": "..."}`.

**Response:** `200 OK` with the [variable](#variables-endpoints).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `value` missing |
| 404 | No such key |

### DELETE /api/v1/workspaces/:slug/vars/:key

Delete a workspace's variable.

**Access:** `vars:write`; write access.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | No such key |

### GET /api/v1/workspaces/:slug/vars/resolved

Resolve the variables a workspace sees by merging three tiers: workspace values
override organization values, which override the owner's user values.

**Access:** `vars:read`; read access.

**Response:** `200 OK` with an array sorted by key ignoring case:

```json
[
  {
    "key": "MY_VAR",
    "value": "workspace-override",
    "origin": "workspace",
    "created_at": "2024-01-01T00:00:00Z",
    "updated_at": "2024-01-01T00:00:00Z"
  }
]
```

`origin` says which tier the value came from: `"user"`, `"org"` or
`"workspace"`. The user tier is the workspace **owner's**, and the organization
tier is used only when the owner is a member of the workspace's organization; a
member of the organization therefore gets the same answer as the owner.

**Errors:** only the [common answers](#common-answers).

---

## Sandbox Profile Endpoints

A hub with a `[sandbox]` section in its `config.toml` lends agents sandboxes:
isolated containers of a shape the operator defines as a **profile** (see
[Server Configuration](configuration.md#sandbox) and
[ADR 03](adr/03-hub-is-the-sandbox-gateway.md)). Without that section every
sandbox route answers `404 sandboxes_disabled`, to every authenticated caller
and before any scope is checked.

### GET /api/v1/sandbox-profiles

List the deployment's profiles, so a caller can choose one by name.

**Access:** `sandboxes:read`. Every authorised caller gets the same list,
whatever their workspaces, user or organization; a bound token may call it.

**Response:** `200 OK`

```json
{
  "profiles": [
    {
      "name": "agent",
      "default": true,
      "image": "registry.example.com/agent:1.4",
      "network": "none",
      "ttl_seconds": 1200,
      "resources": {
        "cpus": 2,
        "memory_mb": 4096,
        "pids": 512,
        "disk_mb": 0
      },
      "secrets": ["ANTHROPIC_API_KEY", "GIT_PAT"],
      "optional_secrets": ["GH_TOKEN"],
      "env_keys": ["LANG", "TZ"],
      "require_landlock": true
    }
  ]
}
```

| Field | Type | Description |
|-------|------|-------------|
| `profiles` | array | Every profile, sorted by `name` |
| `profiles[].name` | string | The name a caller chooses |
| `profiles[].default` | boolean | `true` for the profile marked as the default (at most one is) |
| `profiles[].image` | string | The container image |
| `profiles[].network` | string | `none`, `allowlist` or `unrestricted` |
| `profiles[].ttl_seconds` | integer | The profile's lifetime in whole seconds, rounded down |
| `profiles[].resources` | object | `cpus` (number), `memory_mb`, `pids` and `disk_mb` (integers); `0` means the provider's default |
| `profiles[].secrets` | string[] | Names of the secrets the sandbox receives, sorted; `[]` when none |
| `profiles[].optional_secrets` | string[] | Names of the secrets it receives when they exist, sorted; `[]` when none |
| `profiles[].env_keys` | string[] | Keys of the profile's fixed environment, sorted; `[]` when none. Values are never returned |
| `profiles[].require_landlock` | boolean | Whether the sandbox must confine its agent with Landlock |

The list never contains a secret value or an environment value, and shows
nothing of the provider, its options or the deployment's limits. It reflects
the configuration the hub checked when it started, and it keeps answering when
the container engine is unreachable.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | `sandboxes_disabled` (`sandboxes are not enabled on this hub`), checked before the scope |

---

## Sandbox Lease Endpoints

A **lease** is a caller's right to one sandbox in a workspace. Issuing one fixes
the sandbox's shape from a profile, confirms that the secrets the profile
requires exist for the workspace, enforces the deployment's limit on live
leases and returns a **token**: a bearer credential for that one sandbox,
returned once. The holder then creates the sandbox through the hub's
[sandbox gateway](#the-sandbox-gateway) with the token, and ending the lease
destroys the sandbox.

**States.** A lease is `leased` (issued, no sandbox yet), `active` (its sandbox
exists), `destroyed`, `expired` (no sandbox was created before its
`lease_expires_at`, or its time ran out; see
[Lease lifetime](configuration.md#lease-lifetime)) or `failed` (its sandbox
could not be created). `destroyed`, `expired` and `failed` are terminal: a
terminal lease stays readable for 24 hours after it ended and is then removed,
after which it is `404`.

**Order of checks** on every lease route, the first failing check answering:

1. No credential: `401`.
2. No `[sandbox]` section: `404 sandboxes_disabled`.
3. A PAT without the scope: `403`, before the workspace is looked up. `POST`
   and `DELETE` need `sandboxes:write`, the `GET` routes `sandboxes:read`.
4. The workspace: `404 workspace not found` when it does not exist or the
   caller may not see it. A member of its organization may list and read its
   leases (with the owner's view, secret names included) and gets
   `403 forbidden` on `POST` and `DELETE`.
5. `POST` only: an archived workspace is `400 workspace is archived`.

### The lease representation

Every response that shows a lease has exactly these fields. No response but
the issuing `POST` carries the token, and none carries an environment value.

| Field | Type | Description |
|-------|------|-------------|
| `id` | string | `sbx_` and 32 lowercase hex characters |
| `workspace` | string | The workspace slug |
| `profile` | string | The profile the lease was issued from |
| `purpose` | string | The caller's purpose text; `""` when none |
| `state` | string | `leased`, `active`, `destroyed`, `expired` or `failed` |
| `gateway_url` | string | Where the holder reaches the [sandbox gateway](#the-sandbox-gateway): `[sandbox].external_url` when set, otherwise `http://` or `https://` and the gateway's listen address. The same on every lease |
| `expires_at` | string | `created_at` plus the lease's TTL. The sandbox's lifetime ends here, and the token is refused from this moment in every state |
| `lease_expires_at` | string or `null` | The deadline for creating the sandbox: the earlier of `created_at` plus `[sandbox].lease_timeout` and `expires_at`; it never changes. A lease still `leased` at this moment becomes `expired`. `null` on leases issued by older hubs, which lapse at `expires_at` |
| `image` | string | The profile's image when the lease was issued |
| `network` | string | The profile's network mode |
| `resources` | object | `cpus` (number), `memory_mb`, `pids`, `disk_mb` (integers); `0` means the provider's default |
| `require_landlock` | boolean | Whether the sandbox must confine its agent with Landlock |
| `secrets` | string[] | Names of the required secrets; `[]` when none |
| `optional_secrets` | string[] | Names of the optional secrets; `[]` when none |
| `labels` | object | The caller's labels as given; `{}` when none |
| `created_by` | string | The caller's user id, or `admin` for an admin token |
| `created_at`, `updated_at` | string | When the lease was issued and last changed |
| `sandbox_id` | string or `null` | The sandbox's id once the lease is `active` |
| `error` | string or `null` | Why a `failed` lease failed. A message from the provider has secret values, fixed environment values and URL userinfo removed and is cut to 1024 bytes |
| `ended_at` | string or `null` | When the lease ended |
| `end_reason` | string or `null` | Why it ended: `client` (the holder closed its sandbox through the gateway), `api` (`DELETE`), `expired` (time ran out: either no sandbox was created in time, or the sandbox of an `active` lease was destroyed at `expires_at`; `state` tells the two apart), `shutdown` (the hub shut down, or a `DELETE` or close happened while it was shutting down) or `restart` (the hub found the lease `active` after a crash). A `failed` lease has none |

The profile is copied onto the lease, so a configuration change does not alter
an existing lease.

### POST /api/v1/workspaces/:slug/sandboxes

Issue a lease in a workspace.

**Access:** `sandboxes:write`; write access.

**Request:** an optional JSON object (an empty body counts as `{}`). Any field
other than these four is refused, so that a caller cannot believe it set an
image or an environment; names are matched exactly (`PROFILE` is not
`profile`), and anything after the object is refused.

| Field | Type | Description |
|-------|------|-------------|
| `profile` | string | Profile name, matched exactly. Defaults to the profile marked `default` |
| `purpose` | string | At most 64 characters, no control character. Defaults to `""` |
| `ttl` | string | A duration such as `30m` (Go duration syntax): at least `1m`, at most the profile's TTL. Defaults to the profile's TTL. Fractions of a second are dropped |
| `labels` | object | At most 16 string pairs. A key has 1 to 63 characters from `A-Za-z0-9._-`, starts and ends with a letter or digit, and is not `lease` or `workspace` in any case. A value has at most 256 bytes and no control character |

**Required secrets.** Before the lease is stored, every secret the profile
requires must exist in at least one scope searched for the workspace, in this
order: the workspace, its organization (only when its owner is a member of it)
and the workspace **owner's** user scope, never the caller's. Names match
ignoring case; values are never read. Optional secrets are not checked. If any
is missing the answer is `422 secret_missing`, nothing is stored, and the
message names every missing key in the profile's order and the scope types
searched (and no owner or organization identifier), for example
`required secrets not found: A_KEY, C_KEY (searched scopes: workspace, user)`.

**Capacity.** `[sandbox].max_sandboxes` (default 16) limits the leases live at
once across all workspaces. A lease counts while it is `leased` or `active` and
its `expires_at` is in the future; a `leased` lease past its `lease_expires_at`
no longer counts, even before a sweep ends it. At the limit the answer is
`409 sandbox_capacity`, for example `sandbox capacity reached: at most 2 leases
may be live at once (max_sandboxes)`. Concurrent requests never exceed the
limit.

**Response:** `201 Created`, the [lease](#the-lease-representation) plus
`token`. Emits `hub.sandbox.lease`.

```json
{
  "id": "sbx_3f2a9c0e7b1d4c5a8e6f0b2d1c3a4e5f",
  "workspace": "widgets",
  "profile": "tools",
  "purpose": "triage",
  "state": "leased",
  "gateway_url": "https://sandboxes.example.com",
  "expires_at": "2030-01-02T04:04:05Z",
  "lease_expires_at": "2030-01-02T03:09:05Z",
  "image": "registry.example.com/tools:1",
  "network": "none",
  "resources": {"cpus": 1, "memory_mb": 512, "pids": 100, "disk_mb": 0},
  "require_landlock": true,
  "secrets": ["GIT_PAT"],
  "optional_secrets": [],
  "labels": {"team": "red"},
  "created_by": "user-1",
  "created_at": "2030-01-02T03:04:05Z",
  "updated_at": "2030-01-02T03:04:05Z",
  "sandbox_id": null,
  "error": null,
  "ended_at": null,
  "end_reason": null,
  "token": "sbl_AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"
}
```

`token` is `sbl_` and 44 characters (33 random bytes in unpadded URL-safe
base64), 48 characters in all. The hub keeps only a hash of it.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | A body that is not a JSON object or cannot be read; an unknown field (named); an unknown profile, or none given when there is no default (the message lists the profiles); a bad `ttl` (the message names the profile's TTL); a bad `purpose`; bad `labels` (the message names the key); an archived workspace |
| 404 | `sandboxes_disabled` |
| 409 | `sandbox_capacity` |
| 422 | `secret_missing` |
| 500 | A database or random-source failure; nothing is stored |

### GET /api/v1/workspaces/:slug/sandboxes

List the workspace's leases, newest first, at most 200, as a bare array (`[]`
when there are none). Only live leases (`leased`, `active`) are listed unless
`?all=true`, which adds those that ended in the last 24 hours. Works on an
archived workspace.

**Access:** `sandboxes:read`; read access.

**Request:** `all` accepts `1`, `t`, `T`, `TRUE`, `true`, `True`, `0`, `f`,
`F`, `FALSE`, `false` and `False`; anything else, an empty value included, is
`400`.

**Response:** `200 OK` with an array of [leases](#the-lease-representation).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `all` cannot be parsed |
| 404 | `sandboxes_disabled` |

### GET /api/v1/workspaces/:slug/sandboxes/:id

Read one lease of the workspace, in any state. Works on an archived workspace.

**Access:** `sandboxes:read`; read access.

**Response:** `200 OK` with the [lease](#the-lease-representation).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | `sandboxes_disabled`; `sandbox lease not found` for an `id` that is malformed, unknown, of another workspace, or ended more than 24 hours ago, without saying which |

### DELETE /api/v1/workspaces/:slug/sandboxes/:id

End a lease, destroy its sandbox and revoke its token. Works on an archived
workspace.

**Access:** `sandboxes:write`; write access.

**Behaviour:** the lease becomes `destroyed` with `end_reason` `api`
(`shutdown` while the hub is shutting down), and its token stops working with
the very next gateway call, calls relayed to an existing sandbox included.

- **A `leased` lease** has no sandbox and is ended without calling the
  provider. If its holder's create is in flight, the sandbox it makes is
  destroyed as soon as it exists, and the create fails.
- **An `active` lease** has its sandbox destroyed first, within 30 seconds, and
  is ended only when that worked; a sandbox that is already gone counts as
  destroyed. If the provider fails or does not answer in time the answer is
  `502 sandbox_provider_error` with the provider's message (secret values,
  environment values and URL userinfo removed): nothing has changed, the token
  still works, and the same `DELETE` can be repeated. After a hub restart the
  hub no longer reaches the sandbox of a lease that was `active` before, so a
  `DELETE` ends that lease without destroying the sandbox, which then lives
  until the provider's TTL (a lease `active` at a crash is ended with
  `end_reason` `restart` at the next start in any case).
- **A lease that has ended** is left as it is, with its original `ended_at`
  and `end_reason`: `204`, no provider call, no event. A retry is harmless.

If the lease changes state while the hub works on it (its create finished, or
the holder closed the sandbox), the hub acts on the new state.

**Response:** `204 No Content`. Each change to `destroyed` emits one
`hub.sandbox.destroy` with `reason` `api` (or `shutdown`) and the caller as
actor.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | `sandboxes_disabled`; `sandbox lease not found`, as for `GET` |
| 500 | A database failure |
| 502 | `sandbox_provider_error`; the lease is unchanged |

### GET /api/v1/sandboxes

List the leases of every workspace, with the representation, order, cap and
`?all=true` behaviour of the [workspace list](#get-apiv1workspacesslugsandboxes).

**Access:** admin token only. Any other credential, an API key or a PAT with
both sandbox scopes included, gets `403`.

**Response:** `200 OK` with an array of [leases](#the-lease-representation).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `all` cannot be parsed |
| 403 | Not an admin token |
| 404 | `sandboxes_disabled` |

### The sandbox gateway

The hub serves an AgentKit sandbox gateway on a listener of its own
(`[sandbox].listen`; see
[Server Configuration](configuration.md#the-sandbox-gateway)). It is a gRPC
service, not part of the REST API, and is reached with AgentKit's client at the
lease's `gateway_url`:

```go
p, err := gateway.Dial(ctx, gatewayURL, gateway.Options{Token: token})
sb, err := p.Create(ctx, sandbox.Spec{})
```

**Authentication.** The gateway accepts a lease token and nothing else, on every
call, `Dial` included. A token is accepted only for a lease that is `leased` or
`active` and whose `expires_at` is in the future. Any other token (one the hub
never issued, or that of a lease that is `destroyed`, `expired`, `failed` or
out of time) is refused with gRPC `Unauthenticated`, without saying which, from
the very next call.

**Creating the sandbox.** The hub builds the sandbox from the lease, not from
the spec the caller sends:

| Of the sandbox | Comes from |
|----------------|------------|
| Image, network, resources, Landlock setting, fixed environment | The lease (the profile as it was at issue) |
| Secrets | The profile's `secrets` and `optional_secrets`, read when the sandbox is created from the workspace, then its organization (only if the owner is a member), then the owner's user scope; the first scope that has the name wins. They are set in the environment under the profile's spelling of the name |
| TTL | The time the lease has left, in whole seconds |
| Working directory | The caller's `Workdir` |
| Labels | The caller's labels without every key that starts with `hub.`, the lease's labels as `hub.<key>`, and `hub.lease` and `hub.workspace` |

The caller's image, environment, network, resources, TTL and Landlock setting
are ignored. The spec the gateway reports back has every environment value
replaced by `[redacted]`.

Only a `leased` lease can create, and only one create can be under way per
lease:

| Outcome | Lease | Event |
|---------|-------|-------|
| The provider made the sandbox | `leased` to `active`, `sandbox_id` set | `hub.sandbox.create` |
| The profile is no longer configured; a required secret exists in no scope; the spec cannot be built; the provider failed | `leased` to `failed`, `error` set | `hub.sandbox.failed` |
| The lease's `lease_expires_at` (or `expires_at`) has passed; nothing is created | `leased` to `expired`, `end_reason` `expired` | `hub.sandbox.expire` |
| A `DELETE` or a sweep ended the lease while the provider worked | keeps its terminal state; the new sandbox is destroyed | none |
| The secrets store failed | stays `leased`, so the call can be repeated | none |

A failed create reaches the client as gRPC `Unavailable` with one of these
messages: `lease <id> is <state>: only a leased lease can create its box`,
`lease <id> has expired`, `a Create for lease <id> is already in progress`,
`profile "<name>" is no longer configured`, `secret_missing: <KEYS> (searched
scopes: workspace, org, user)` (the scope types actually searched),
`hub is shutting down and creates no more sandboxes`, or the provider's error,
scrubbed as for `error`. A second create with the same token therefore fails
while the lease is `active`, and is `Unauthenticated` once it has ended.

**Using and ending the sandbox.** The gateway relays the holder's calls to its
sandbox, lists only that sandbox, and answers a request to destroy any other
sandbox as it answers an unknown id. Its `Reap` call does nothing for a lease
holder (the hub sweeps leases on its own; see
[Lease lifetime](configuration.md#lease-lifetime)). When the holder closes its
sandbox, the hub destroys it and the lease goes from `active` to `destroyed`
with `end_reason` `client` (one `hub.sandbox.destroy`); if the provider cannot
destroy it, the client gets the scrubbed error and the lease stays `active`.
Closing a sandbox in the last two seconds of the lease's time, or after it,
closes it and changes nothing else: the sandbox's own TTL ends a moment before
the lease, and the next sweep ends the lease as `expired`.

---

## Forge Endpoints

A workspace's repository lives on a forge, GitHub or GitLab. These routes let a
caller read the repository, its issues and its pull requests through the hub,
and change issues, comments, labels and pull requests, using the credential the
hub holds for the workspace, never the caller's own (see
[ADR 04](adr/04-hub-is-the-workspaces-forge-client.md)). Neither the request's
`Authorization` header nor its body as sent reaches the forge: a read takes
only the remote to read (`target`) and its own parameters, and a write sends
only the fields of its body. No response shows the credential, only its kind.

| | Reads | Writes | Webhook routes |
|---|---|---|---|
| Routes | [`GET .../forge`](#get-apiv1workspacesslugforge) and the nine [read routes](#the-read-routes) | The nine [write routes](#the-write-routes) | [`PUT`, `DELETE .../forge/webhook`, `GET .../forge/deliveries`](#the-webhook-routes) |
| Scope | `forge:read` | `forge:write` | `forge:write`; the delivery log `forge:read` |
| Workspace access | read | write | write; the delivery log read |
| Workspace status | any, archived included | not archived | `PUT` not archived; `DELETE` and the log any |
| Audit event | none | one `hub.forge.*` per write that reached the forge | `hub.forge.webhook.set` / `.unset` |

None of them needs the workspace's clone or takes the [workspace lock](#workspace-lock).

A forge can also tell the hub what happened on the repository, so that a
consumer learns of an issue, a label, a comment, a pull request or a push
without polling: the forge posts a **delivery** to the hub's
[webhook ingress](#the-webhook-ingress), which checks it against a secret it
holds for the workspace, records it once and relays it as a `hub.forge.*` audit
event, which reaches the [event stream](#get-apiv1events) and the
[audit query](#get-apiv1audit). The hub never acts on a delivery and never
contacts the forge for one.

### How the hub reaches the forge

| | origin (the default) | upstream (`?target=upstream`) |
|---|---|---|
| Remote URL | the workspace's `git_url` | the `upstream_url` of a `carry_patch` workspace |
| Credential | `GIT_PAT`, or `GIT_USERNAME` with `GIT_PASSWORD` | `UPSTREAM_GIT_PAT`, or `UPSTREAM_GIT_USERNAME` with `UPSTREAM_GIT_PASSWORD`, else the origin's |
| API URL setting | `forge_api_url` | `upstream_forge_api_url`, with no fallback to `forge_api_url` |

- **Repository.** It is read from the remote URL (`https://host/owner/name`,
  `ssh://...` or `git@host:owner/name`, with `.git` and userinfo removed).
  GitLab subgroups are kept: for `https://gitlab.example.com/group/subgroup/widgets`
  the owner is `group/subgroup` and the name `widgets`.
- **Which forge.** A host whose name contains `github` or `gitlab` says which;
  so does an API URL setting that contains one of the two words. Without a
  setting the API URL is `https://api.github.com` for `github.com`,
  `https://<host>/api/v3` for another GitHub host and `https://<host>/api/v4`
  for a GitLab host. When neither the host nor a setting tells the forge, the
  answer is `422 forge_unknown` and nothing is sent. For reads, the hub
  deployment's own `GITHUB_API_URL` and `GITLAB_API_URL` environment variables
  also apply; writes ignore them.
- **API URL settings.** `forge_api_url` and `upstream_forge_api_url` are
  [workspace settings](#workspace-settings-reference). A value that is set must
  be an absolute `https://` URL with a host and without userinfo, query or
  fragment; anything else is `422 forge_api_url_invalid`, with a message that
  names the setting and the rule but not the value. Surrounding white space and
  trailing slashes are ignored; a GitLab value gets `/api/v4` appended when it
  does not end with it.
- **Credential kind.** `pat` for a `GIT_PAT`; `basic` for a username and
  password, in which case the password is sent to the forge as the token;
  `none` when there is no credential, in which case the request carries no
  token, whatever token the hub's own environment (`GITHUB_TOKEN`, `GH_TOKEN`,
  `GITLAB_TOKEN`) holds.
- **Nothing is cached**: a rotated secret or a changed setting applies to the
  next call.
- **Every request** to the forge carries `User-Agent: af-hub/<version>`, follows
  the deployment's proxy settings and refuses a redirect to another host, port
  or scheme (`502 forge_error`). A call, retries included, has 30 seconds
  (`504 forge_timeout`).

### Order of checks

A read route checks, in this order:

1. No credential: `401`.
2. A PAT without `forge:read` (or `forge:write`): `403 PAT requires forge:read
   scope`, before the workspace is looked up, so also for a slug that does not
   exist. An unknown credential type is `403` too.
3. The workspace: `404 workspace not found` (a member of its organization reads
   with the workspace's credential, as the owner does).
4. `target` (every route but `GET .../forge`): `400`.
5. The path and query parameters: `400`, naming the parameter.
6. Working out the forge and its client: `422` or `500`.

A write route checks, in this order:

1. No credential: `401`.
2. A PAT without `forge:write` (`forge:read` alone included):
   `403 PAT requires forge:write scope`, before the workspace is looked up.
3. The workspace: `404 workspace not found`; a member gets `403 forbidden`.
4. An archived workspace: `400 workspace is archived`.
5. The request: `400` for a bad `target` (`workspace_mode_mismatch` for
   `upstream` on a workspace that is not `carry_patch` with an
   `upstream_url`), a path number that is not a positive integer, a body that
   is not JSON of the route's shape, or a field that breaks the route's rules.
6. Working out the forge and its client: `422` or `500`. A request this forge
   cannot take is refused here (an update that sets only `assignees` on GitLab
   is `400 nothing to update`).
7. A body-like field over the forge's limit: `413 body_too_large`. The limit
   depends on the forge, so a forge the hub cannot tell is `422` even for an
   oversized body.

A request refused at any of these steps sends nothing to the forge and records
no event.

### The target

Every forge route but [`GET .../forge`](#get-apiv1workspacesslugforge) and the
webhook routes takes `?target=`:

| Value | Works on |
|---|---|
| absent, or `origin` | the repository at the workspace's `git_url` |
| `upstream` | the repository at `upstream_url`; only for a `carry_patch` workspace with a non-empty `upstream_url`, else `400 workspace_mode_mismatch` |

Any other value (`ORIGIN`, an empty value, a repeated parameter) is `400`
`target must be "origin" or "upstream"`.

### GET /api/v1/workspaces/:slug/forge

Describe how the hub reaches the forge, without contacting it. Useful when the
other routes answer `forge_unknown`.

**Access:** `forge:read`; read access; any workspace status.

**Response:** `200 OK`

```json
{
  "forge": "github",
  "api_url": "https://api.github.com",
  "repository": { "host": "github.com", "owner": "acme", "name": "widgets" },
  "credential": "pat",
  "upstream": null,
  "webhook": {
    "configured": true,
    "url": "https://hub.example.com/hooks/forge/widgets",
    "last_delivery_at": "2026-10-21T09:12:03Z"
  }
}
```

| Field | Type | Description |
|-------|------|-------------|
| `forge` | string | `github`, `gitlab` or `unknown` (an unclassifiable host is reported, not an error) |
| `api_url` | string or null | The API URL the hub uses (for GitLab, with `/api/v4`); `null` when the forge is `unknown`. It is the setting or the host's default, so a `GITHUB_API_URL` or `GITLAB_API_URL` in the hub's environment is not shown |
| `repository` | object or null | `host`, `owner` (the full namespace path) and `name`; `null` when the remote URL names no repository |
| `credential` | string | `pat`, `basic` or `none`: the kind, never a value |
| `upstream` | object or null | The same four fields for `upstream_url` (upstream credential, `upstream_forge_api_url`); `null` unless the workspace is `carry_patch` with a non-empty `upstream_url` |
| `webhook.configured` | boolean | Whether the workspace has the secret `FORGE_WEBHOOK_SECRET` (workspace scope only) |
| `webhook.url` | string or null | Where the forge is to post: `server.external_url` without a trailing `/`, then `/hooks/forge/<slug>`; `null` when `external_url` is not set |
| `webhook.last_delivery_at` | string or null | When the hub received the workspace's latest delivery, of any outcome |

The `webhook` block never holds the secret.

**Errors:**

| Status | Condition |
|--------|-----------|
| 422 | `forge_api_url_invalid` (`upstream_forge_api_url` for the upstream block) |
| 500 | The secrets store or the delivery log could not be read (no partial answer) |

### The read routes

Each route reads one thing from the forge and returns it. A list is a bare JSON
array, `[]` when empty, never `null`.

| Route | Response |
|---|---|
| `GET /api/v1/workspaces/:slug/forge/repository` | the repository |
| `GET /api/v1/workspaces/:slug/issues` | an array of issues |
| `GET /api/v1/workspaces/:slug/issues/:n` | an issue thread: the issue with its comments |
| `GET /api/v1/workspaces/:slug/issues/:n/comments` | an array of comments |
| `GET /api/v1/workspaces/:slug/pulls/:n` | the pull request |
| `GET /api/v1/workspaces/:slug/pulls/:n/state` | the pull request's state |
| `GET /api/v1/workspaces/:slug/pulls/:n/files` | an array of changed files |
| `GET /api/v1/workspaces/:slug/pulls/:n/checks` | an array of CI check runs |
| `GET /api/v1/workspaces/:slug/pulls/:n/reviews` | an array of reviews |

**Access:** `forge:read`; read access; any workspace status. Every route takes
[`?target=`](#the-target).

**Request:** `:n` is a positive decimal integer (`400 n must be a positive
decimal integer`); on GitLab it is the project's `iid`. `GET .../issues` takes:

| Parameter | Values | Sent to the forge |
|-----------|--------|-------------------|
| `state` | `open`, `closed`, `all` | when given |
| `labels` | comma-separated; empty items are dropped | when any remain |
| `assignee` | free text | when given |
| `sort` | `created`, `updated` | when given |
| `direction` | `asc`, `desc` | when given |
| `limit` | 1 to 100; default 30 | always |

A parameter not given is not sent, and the forge's default applies. A bad,
empty or repeated value is `400` with a message that starts with the
parameter's name (`state must be one of: open, closed, all`, `limit must be an
integer from 1 to 100`).

**Response:** `200 OK`. The result types (fields marked `?` are omitted when
empty):

| Type | Fields |
|---|---|
| repository | `full_name`, `default_branch`, `private`, `archived`, `permissions` (`push`, `pull`, `admin`), `allow_merge_commit?`, `allow_squash_merge?`, `allow_rebase_merge?` |
| issue | `number`, `title`, `body`, `state`, `url?`, `html_url?`, `author` (`login`), `labels?`, `created_at`, `updated_at`, `closed_at?`, `is_pr` |
| issue thread | `issue`, `comments`, `truncated`, `comments_err?` |
| comment | `id?`, `body`, `user` (`login`), `created_at`, `updated_at?`, `url?`, `html_url?` |
| pull request | `number`, `title`, `body`, `state`, `url?`, `html_url?`, `draft`, `merged`, `head_sha`, `head_branch`, `base_branch`, `author`, `created_at`, `updated_at`, `closed_at?`, `merged_at?` |
| pull request state | `number`, `state`, `merged`, `head_sha` |
| changed file | `filename`, `status`, `additions`, `deletions` |
| check run | `name`, `status`, `conclusion`, `summary?`, `url?` |
| review | `author`, `state`, `body`, `submitted_at` |
| label | `name`, `color?`, `description?` (GitHub gives the color without `#`, GitLab with) |

In an issue thread, `truncated` says that more comments exist than were read.
When the issue was read but its comments could not be, the thread carries the
error in `comments_err` (with the token and URL userinfo removed) and the
answer is still `200`. The list routes return the bare array and do not say
whether the list was cut short.

**Example:** `GET /api/v1/workspaces/widgets/issues?labels=af:triage&state=open`

```json
[
  {
    "number": 12,
    "title": "Widgets break",
    "body": "it breaks",
    "state": "open",
    "html_url": "https://github.com/acme/widgets/issues/12",
    "author": { "login": "alice" },
    "labels": ["af:triage"],
    "created_at": "2026-01-02T03:04:05Z",
    "updated_at": "2026-01-02T03:04:05Z",
    "is_pr": false
  }
]
```

**Errors:** see [Order of checks](#order-of-checks) and
[Failures of the forge](#failures-of-the-forge).

### The write routes

| Route | Body | Success |
|---|---|---|
| `POST /api/v1/workspaces/:slug/issues` | `{title, body?, labels?, assignees?}` | 201, the issue |
| `PATCH /api/v1/workspaces/:slug/issues/:n` | `{title?, body?, state?, labels?, assignees?}` | 200, the issue |
| `POST /api/v1/workspaces/:slug/issues/:n/close` | `{comment?}` or no body | 200, the issue |
| `POST /api/v1/workspaces/:slug/issues/:n/comments` | `{body}` | 201, `{"url": …}` |
| `POST /api/v1/workspaces/:slug/issues/:n/labels` | `["name", …]` | 204, no body |
| `DELETE /api/v1/workspaces/:slug/issues/:n/labels/:name` | none | 204, no body |
| `POST /api/v1/workspaces/:slug/labels` | `{name, color?, description?}` | 201, the label; 200 when it exists |
| `POST /api/v1/workspaces/:slug/pulls` | `{title, body?, head, base, draft?}` | 201, the pull request |
| `POST /api/v1/workspaces/:slug/pulls/:n/comments` | `{body}` | 201, `{"url": …}` |

**Access:** `forge:write`; write access; the workspace must not be archived.
Every route takes [`?target=`](#the-target). Answers use the types of the
[read routes](#the-read-routes); fields the hub does not know are ignored;
`:n` is the issue number (GitLab: the `iid`).

- **`POST .../issues`.** `title` is required and not blank; `body` may be
  empty; `labels` and `assignees` hold at most 100 names each, none blank. On
  GitLab, `assignees` are accepted and not sent (GitLab takes user ids).
- **`PATCH .../issues/:n`.** Only the fields given change; a field absent or
  `null` is left alone. `title` must not be blank; `body: ""` clears the body;
  `state` is `open` or `closed`; `labels` and `assignees` replace the whole set,
  and `[]` removes every entry. A body that sets none of them is
  `400 nothing to update`, and so, on GitLab, is one that sets only `assignees`
  (an update that sets other fields records only those as changed). The answer
  is the issue as the forge returns it.
- **`POST .../issues/:n/close`.** No body, `{}` or a blank comment just closes
  the issue. A comment that is not blank is posted first, and the issue is
  closed only if that worked: a refused comment leaves the issue open and is
  answered with its error; a close that fails after the comment leaves the
  comment, is answered with the close's error, and the event holds the
  comment's URL. Closing a closed issue is `200`. The answer is the issue after
  the close.
- **`POST .../issues/:n/comments`.** `body` is required and not blank.
- **`POST .../issues/:n/labels`.** A bare JSON array of 1 to 100 names, none
  blank (an object, an empty array or a non-string entry is `400`). The labels
  are added and the issue keeps its others; a name the repository does not
  have is the forge's to deal with, and its answer is passed on.
- **`DELETE .../issues/:n/labels/:name`.** `:name` is the last path segment,
  decoded exactly once: `type%2Fbug` removes `type/bug`, `af:fix` is
  unchanged; a label named `.` or `..` is sent as `%2E` or `%2E%2E`. The answer
  is `204` also when the issue never had the label; when the forge answers
  not found, the hub reads the issue once and answers `204` if it exists and
  `404 forge_not_found` if not.
- **`POST .../labels`.** `name` is required and not blank; `color` is six hex
  digits with or without a leading `#` (default `ededed`); `description` is
  optional. A label that exists is answered `200` as the forge holds it and is
  never changed, so ensuring a label twice is harmless; one that does not is
  created and answered `201`. If the create is refused (for example because a
  concurrent call just created the label), the hub looks once more and answers
  `200` if the label is there, else the create's error.
- **`POST .../pulls`.** `title`, `head` and `base` are required and not blank;
  `body` is optional; `draft` is a boolean, `false` when absent. `head` is sent
  as given, so GitHub's `owner:branch` opens a pull request from a fork. With
  `?target=upstream` the pull request is opened on the upstream repository with
  the upstream credential. A pull request that exists for `head` and `base` is
  refused by the forge (`422 forge_rejected`); the hub keeps no idempotency key,
  so a retried create can create twice. To get a branch of the hub's clone to
  the forge first, use [branch push](#post-apiv1workspacesslugbranchesnamepush).
- **`POST .../pulls/:n/comments`.** `body` is required and not blank. The
  comment goes to the pull request's conversation (on GitLab, the merge
  request's notes).

**Body limits.** The `body` of an issue (create and update), the `comment` of a
close, the `body` of a comment and the `body` of a pull request are measured in
Unicode code points:

| Forge | Limit |
|---|---|
| GitHub | 65,536 |
| GitLab | 1,000,000 |

A field over the limit is `413 body_too_large` with a message such as
`body is 70000 characters; github accepts at most 65536`, and nothing is sent.
Titles, `head`, `base`, label names and descriptions and assignee names are not
measured; the forge's refusal of them is answered like any other.

**Retries and cancellation.** Creating an issue, commenting, adding labels and
opening a pull request wait out one rate limit (as reads do) and try once more;
the other writes are not retried, and the hub adds no retry of its own, so a
create that times out or cannot reach the forge may still have happened. A
write whose caller goes away is finished and recorded all the same. A write
that takes several requests (a close with a comment, a label lookup and create,
a label removal and its read) gives each request the full 30 seconds.

**Audit.** A write that sent at least one request to the forge records exactly
one `hub.forge.*` event, successful or not, before it answers (see
[Hub Audit Events](#hub-audit-events)).

**Errors:** see [Order of checks](#order-of-checks) and
[Failures of the forge](#failures-of-the-forge).

### The webhook routes

| Route | Body | Success |
|---|---|---|
| `PUT /api/v1/workspaces/:slug/forge/webhook` | `{"secret"?: string}`, or no body | 200, `{"configured": true, "url": …, "secret"?: …}` |
| `DELETE /api/v1/workspaces/:slug/forge/webhook` | none | 204, no body |
| `GET /api/v1/workspaces/:slug/forge/deliveries?since=&limit=` | none | 200, an array |

A forge's webhook is registered by a person: the hub gives the URL and the
secret to enter in the forge's webhook settings and does not call the forge.
The secret is the workspace secret `FORGE_WEBHOOK_SECRET`; `PUT` is one way to
set it, `afc secrets create FORGE_WEBHOOK_SECRET --workspace <slug>` another.
The hub reads it for every delivery, so a rotated secret applies from the next
one. A generated secret's value appears in the response of the `PUT` that
generated it and nowhere else: no other response, log line, audit event or
delivery record carries it.

#### PUT /api/v1/workspaces/:slug/forge/webhook

Set the workspace's webhook secret, generating one unless the caller supplies
it.

**Access:** `forge:write` (a PAT without it: `403 PAT requires forge:write
scope`); write access; the workspace must not be archived. Checked in that
order, then the body.

**Request:** an optional JSON object. No body, `{}` or `{"secret": null}` makes
the hub generate a secret: 32 random bytes as 64 lowercase hex characters. A
supplied secret must be 16 to 255 characters, each a printable ASCII character
from `!` to `~` (no space or control character, so it is safe in the header
GitLab sends it in).

**Response:** `200 OK`

```json
{
  "configured": true,
  "url": "https://hub.example.com/hooks/forge/widgets",
  "secret": "5d0c…64 hex characters…"
}
```

`url` is `null` when `server.external_url` is not set (the secret is stored all
the same). `secret` is present only when the hub generated it. Emits
`hub.forge.webhook.set` with `{"generated": <bool>}`.

**Errors:** a `400` message names `secret` and the rule, never the value.

| Status | Condition |
|--------|-----------|
| 400 | `the request body must be a JSON object with an optional "secret" string`; `secret must be a string`; `secret must be 16 to 255 characters long`; `secret must only have the printable ASCII characters from ! to ~, so no space and no control character`; the workspace's secrets are full (`maximum of 100 entries per scope exceeded`); the workspace is archived |
| 500 | The secrets store failed; no event is emitted |

#### DELETE /api/v1/workspaces/:slug/forge/webhook

Remove the webhook secret, so that the hub refuses the forge's deliveries
(`404`) from then on. Recorded deliveries stay until they are pruned.

**Access:** `forge:write`; write access; any workspace status.

**Response:** `204 No Content`, whether or not there was a secret. Emits
`hub.forge.webhook.unset` with `{"existed": <bool>}`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 500 | The secrets store failed |

#### GET /api/v1/workspaces/:slug/forge/deliveries

List the deliveries the hub recorded for the workspace, newest first (rows of
the same second last recorded first). The hub keeps them for 7 days.

**Access:** `forge:read` (a PAT without it: `403` naming `forge:read`); read
access; any workspace status.

**Request:**

| Parameter | Description |
|---|---|
| `since` | An RFC 3339 time; only deliveries received then or later. Whole seconds are kept, so a `since` with a fraction starts at the next second |
| `limit` | 1 to 100; default 30 |

A bad or repeated value is `400` naming the parameter (`since must be an RFC
3339 timestamp, for example 2026-10-21T09:00:00Z`, `limit must be an integer
from 1 to 100`, `since must be given once`).

**Response:** `200 OK` with an array, `[]` when there is none:

```json
[
  {
    "delivery_id": "72d3162e-cc78-11e3-81ab-4c9367dc0958",
    "forge": "github",
    "event": "issues",
    "action": "labeled",
    "target": "origin",
    "outcome": "relayed",
    "relayed": ["hub.forge.issue.labeled"],
    "received_at": "2026-10-21T09:12:03Z"
  }
]
```

| Field | Type | Description |
|-------|------|-------------|
| `delivery_id` | string | The forge's id of the delivery |
| `forge` | string | `github` or `gitlab` |
| `event` | string | The forge's event name (`X-GitHub-Event`, `X-Gitlab-Event`), cut to 64 bytes |
| `action` | string | The payload's action; `""` when it has none |
| `target` | string or null | `origin` or `upstream`: which of the workspace's repositories the delivery is about; `null` for neither |
| `outcome` | string | `relayed`, `ignored`, `repository_mismatch` or `malformed` |
| `relayed` | string[] | The events relayed, in order; `[]` for any outcome but `relayed` |
| `received_at` | string | When the hub received it |

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | A bad `since` or `limit` |

### The webhook ingress

`POST /hooks/forge/:slug` receives a workspace forge's deliveries. It is served
at the server root, outside `/api/v1` and **without bearer authentication**:
the forge proves itself with the workspace's secret. The API's body size limit
and content-type check do not apply; the ingress has a limit of its own. It
never contacts the forge and needs no clone.

**Checks**, in this order:

1. **The workspace and its secret.** An unknown slug and a workspace without
   the secret (or with an empty one) both answer `404` with the same body
   (`not found`), before the body is read. A store failure is `500`. The
   workspace's status does not matter: an archived workspace that has a secret
   still relays.
2. **The scheme**, by header: `X-Hub-Signature-256` is GitHub's; otherwise
   `X-Gitlab-Token` is GitLab's; with neither the answer is `401`, before the
   body is read. A header given twice is not valid.
3. **GitLab's token** is compared with the secret, in constant time, before the
   body is read.
4. **The body** is read up to 1 MiB (1,048,576 bytes); more is
   `413 body_too_large`.
5. **GitHub's signature** must be `sha256=` and the 64 lowercase hex digits of
   the HMAC-SHA256 of the raw body under the secret, compared in constant time.

A missing, malformed or wrong signature or token is `401 delivery signature
missing or invalid`, whatever is wrong; nothing is recorded or emitted.

**Identity.** The hub then reads the delivery id and the event name:

| Forge | Delivery id | Event name |
|---|---|---|
| GitHub | `X-GitHub-Delivery` | `X-GitHub-Event` |
| GitLab | `Idempotency-Key`, else `X-Gitlab-Event-UUID` | `X-Gitlab-Event` |

A delivery without either, or with an id longer than 255 characters or with a
character outside printable ASCII (space to `~`), is `400` and nothing is
recorded.

**Each delivery once.** Deliveries are recorded per workspace and delivery id.
A delivery whose id is already recorded is a duplicate: `202
{"outcome":"duplicate"}`, nothing emitted, nothing added, and of several copies
arriving together only one is processed. If recording fails the answer is `500`
and nothing is emitted, so the forge's redelivery is processed in full.

**Answer, then relay.** An accepted delivery is answered `202` with
`{"outcome": …}` before any event is relayed, so a slow audit store cannot hold
the forge's connection open:

| Outcome | Meaning |
|---|---|
| `relayed` | At least one event was relayed |
| `ignored` | The event and action are not ones the hub relays, or the payload has nothing to relay (a note on a commit, an update that changes nothing) |
| `repository_mismatch` | The delivery has events to relay but is about a repository that is neither the workspace's origin nor (on a `carry_patch` workspace with an `upstream_url`) its upstream |
| `duplicate` | The delivery id was recorded before |

A body that is not one JSON object, or lacks what the delivery is about (the
repository; the number of an issue, pull request or merge request; the name of
a label; the ref of a push), is recorded as `malformed` and answered `400`.
Descriptive fields (title, URL, branches, labels) default to empty instead.
Once a delivery is recorded its events are relayed, even if the forge has gone
away by then. The payload itself is never stored or logged.

**Repository.** The payload's repository path (GitHub `repository.full_name`,
GitLab `project.path_with_namespace`) is compared, ignoring case, with the
origin's `owner/name` and, on a `carry_patch` workspace with an `upstream_url`,
with the upstream's: a match with the origin is `target: origin`, one only with
the upstream `target: upstream`.

**Mapping.**

| GitHub (`X-GitHub-Event`, `action`) | Hub event(s) |
|---|---|
| `issues` opened, edited, closed, reopened | `hub.forge.issue.opened`, `.edited`, `.closed`, `.reopened` |
| `issues` labeled, unlabeled | `hub.forge.issue.labeled`, `.unlabeled` |
| `issue_comment` created | `hub.forge.comment.created` (`pull_request: true` when the issue is a pull request) |
| `pull_request` opened, reopened, synchronize | `hub.forge.pull_request.opened`, `.reopened`, `.synchronize` |
| `pull_request` closed | `.merged` when the pull request was merged, else `.closed` |
| `push` | `hub.forge.push` |

| GitLab (`X-Gitlab-Event`, `object_attributes.action`) | Hub event(s) |
|---|---|
| `Issue Hook` open, close, reopen | `hub.forge.issue.opened`, `.closed`, `.reopened` |
| `Issue Hook` update | `.edited` when the title or description changed; then one `.unlabeled` per label removed and one `.labeled` per label added, each group sorted by name. An update with none of these changes is ignored. At most 100 label events are relayed per delivery (`unlabeled` first), and an event lists at most 100 labels |
| `Note Hook` on an `Issue` or a `MergeRequest`, no action or `create` | `hub.forge.comment.created` (`pull_request: true` for a merge request) |
| `Merge Request Hook` open, close, reopen, merge | `hub.forge.pull_request.opened`, `.closed`, `.reopened`, `.merged` |
| `Merge Request Hook` update | `.synchronize` when the payload has `oldrev`; otherwise ignored |
| `Push Hook`, `Tag Push Hook` | `hub.forge.push` |

Everything else is ignored, for example GitHub `ping`, `pull_request` labeled
and `issue_comment` edited, GitLab's confidential hooks, and notes on commits or
snippets. The events and their metadata are in
[Hub Audit Events](#hub-audit-events).

The hub deletes recorded deliveries older than 7 days, and those of workspaces
that no longer exist, every hour.

**Not supported:** Forgejo and Gitea deliveries, GitLab's signing-token header,
GitHub's legacy SHA-1 `X-Hub-Signature` and form-encoded payloads (such a
delivery with a valid signature is recorded `malformed`), a webhook secret of
an organization or a user, and rate limiting of the endpoint.

### Failures of the forge

Every failure of a forge route is answered by one of these:

| Condition | Status | `error_type` |
|---|---|---|
| `target=upstream` on a workspace that is not `carry_patch` with an `upstream_url` | 400 | `workspace_mode_mismatch` |
| An invalid `target`, `:n` or query parameter, or a write body that breaks a rule of its route | 400 | |
| A write on an archived workspace | 400 | |
| A body-like field over the forge's limit (writes) | 413 | `body_too_large` |
| `forge_api_url` / `upstream_forge_api_url` invalid | 422 | `forge_api_url_invalid` |
| The forge cannot be told, or the remote URL names no repository | 422 | `forge_unknown` |
| The forge answered 404 | 404 | `forge_not_found` |
| The forge answered 401 | 502 | `forge_unauthenticated` |
| The forge answered 403 that is not a rate limit | 502 | `forge_forbidden` |
| A rate limit the hub could not wait out | 429 | `forge_rate_limited` |
| The forge answered another 4xx | 422 | `forge_rejected` |
| The forge answered 5xx, an unreadable response, or a redirect to another host | 502 | `forge_error` |
| The connection failed (DNS, TCP, TLS) | 502 | `forge_unreachable` |
| The 30-second deadline passed | 504 | `forge_timeout` |
| The secrets store or the settings could not be read | 500 | |

- The forge's own message is in `message` as `the forge answered <status>:
  <message>`, with the token replaced by `[redacted]`, URL userinfo removed and
  the text cut to 1024 bytes. A `401` for a `basic` credential adds `The forge
  API needs a token: store one as GIT_PAT, or as GIT_PASSWORD.`
- `forge_rate_limited` carries `Retry-After` in whole seconds, rounded up, when
  the wait is known (the forge's own value plus one second). The hub itself
  waits out a backoff of up to 120 seconds and tries once more; the 30-second
  deadline cuts a wait of 29 to 120 seconds short, which is answered
  `504 forge_timeout`.
- A write the forge refuses is answered like a read it refuses, and its event
  carries the same `error_type`.
- A write that the workspace's missing credential prevents (`no authentication
  token configured`) is `502 forge_error`; nothing is sent and no event is
  recorded.
- No response contains the credential or the userinfo of a URL.

---

## Hub Audit Events

The hub records an audit event for each significant operation. Events are read
with the [unified audit query](#get-apiv1audit) and streamed live by the
[event stream](#get-apiv1events). Each event has an `event_type`, an actor
(`actor_id` and `actor_type`: by default the calling credential's user and
type), the `workspace`, a `resource_type`, a `resource_id`, an `action` and
metadata. Recording an event never changes the answer of the request that
caused it.

| Event Type | Resource Type | When Emitted | Metadata |
|------------|---------------|--------------|----------|
| `hub.patch.create` | `patch` | A single patch is added with `POST .../patches` (not for a batch or an `if_not_exists` hit) | `branch_name`, `position`, `branch_resolution` (`local`, `origin_tracking`, `origin_fetch` or `skipped`) |
| `hub.rebuild.followup` | `rebuild` | A follow-up rebuild is queued because a patch tip or the upstream head moved during a rebuild | `submitted_by`, the workspace slug |
| `hub.patch.sync` | `patch` | A `carry_patch` sync with `patch_branch_source` `"origin"` completes (`resource_id` is the workspace slug, `action` `sync`) | `origin_fetched` (bool), `created`, `fast_forwarded`, `replaced`, `diverged`, `missing_on_origin` (branch names) |
| `hub.patch.replace` | `patch` | A patch branch is replaced by such a sync (one per branch) or by a reset-to-origin (`resource_id` is the branch, `action` `replace`) | `branch_name`, `replaced_sha`, `origin_sha`; `trigger: "reset_to_origin"` when a reset caused it (a sync sets no `trigger`) |
| `hub.patch.mirror_failed` | `patch` | Mirroring a pushed patch branch to the fork failed (one per branch) | `branch_name`, `error` (URL userinfo removed) |
| `hub.patch.reset` | `patch` | `POST .../patches/:id/reset-to-origin` reset a branch | `branch_name`, `action` (`none`, `created`, `fast_forwarded` or `replaced`), `local_sha` (absent when there was no local branch), `origin_sha`, `replaced_sha` (only for `replaced`) |
| `hub.workspace.delete` | `workspace` | `DELETE /workspaces/:slug` | `revoked_tokens`: the ids of the bound tokens this deletion revoked, sorted; `[]` when none |
| `hub.token.bind` | `token` | A token minted with `POST /workspaces/:slug/tokens` was bound (`resource_id` is the token id, `action` `bind`); never for a refusal or a rolled-back mint | `token_id`, `workspace`, `permissions`, `expires_at` (`null` when it never expires). Never the token |
| `hub.token.bound_refused` | `token` | A bound token was refused, by the API or the git server (`resource_id` is the token id, `action` `refuse`, `workspace` the bound workspace, actor type `pat`). At most one per token per minute | `token_id`, `bound_workspace`, `requested_workspace` (when the request named one), `method`, `route` (the route template or the git route; `method` may be empty), `status` (403 or 404), `surface` (`api` or `git`), `suppressed` (refusals held back since the token's previous event) |
| `hub.sandbox.lease` | `sandbox` | `POST .../sandboxes` issued a lease (`resource_id` is the lease id, `action` `lease`) | `lease_id`, `profile`, `purpose`, `ttl` (a duration string such as `1h0m0s`), `secret_names`, `optional_secret_names`. Never a secret value, an environment value or the token |
| `hub.sandbox.create` | `sandbox` | The gateway made a lease's sandbox, and the lease became `active` (`action` `create`; actor type `lease`, actor id the lease's `created_by`) | `lease_id`, `sandbox_id`, `image`, `network` |
| `hub.sandbox.failed` | `sandbox` | A lease became `failed` (`action` `fail`; actor type `lease`) | `lease_id`, `error` (scrubbed, at most 1024 bytes) |
| `hub.sandbox.destroy` | `sandbox` | A lease became `destroyed`: its holder closed the sandbox (actor type `lease`), a `DELETE` ended it (the caller is the actor), or the hub did when it shut down or restarted after a crash (actor type `system`, empty actor id). One per change (`action` `destroy`) | `lease_id`, `sandbox_id` (`null` when there was none), `reason` (`client`, `api`, `shutdown` or `restart`) |
| `hub.sandbox.expire` | `sandbox` | Time ended a lease: a `leased` lease got no sandbox before its deadline, or an `active` lease passed its `expires_at` and its sandbox was destroyed. One per lease, with no `hub.sandbox.destroy` (`action` `expire`; actor type `system`) | `lease_id`, `state` (`expired`, or `destroyed` when a sandbox was destroyed), `sandbox_id` (only when a sandbox was destroyed) |
| `hub.forge.issue.create` | `issue` | `POST .../issues` sent a request to the forge, successful or not (`resource_id` is the new number, empty on failure; `action` `create`) | the common forge-write metadata (below) |
| `hub.forge.issue.update` | `issue` | `PATCH .../issues/:n` (`resource_id` `n`, `action` `update`) | `fields`: the fields the request changed, sorted |
| `hub.forge.issue.close` | `issue` | `POST .../issues/:n/close` (`action` `close`); one event also for a close with a comment | `comment_url`: the comment's URL when one was posted, also when the close then failed |
| `hub.forge.comment.create` | `comment` | `POST .../issues/:n/comments` (`resource_id` the issue number, `action` `create`) | `url` is the comment's |
| `hub.forge.label.add` | `label` | `POST .../issues/:n/labels` (`action` `add`) | `labels`: the names given |
| `hub.forge.label.remove` | `label` | `DELETE .../issues/:n/labels/:name` (`action` `remove`) | `label` |
| `hub.forge.label.create` | `label` | `POST .../labels`, whether it created the label or found it (`resource_id` the name, `action` `create`) | `label`; `created`: true only when this call created it |
| `hub.forge.pull_request.create` | `pull_request` | `POST .../pulls` (`resource_id` the new number, empty on failure; `action` `create`) | `head`, `base`, `draft` |
| `hub.forge.pull_request.comment` | `pull_request` | `POST .../pulls/:n/comments` (`action` `comment`) | `url` is the comment's |
| `hub.forge.issue.opened` | `issue` | A forge reported an opened issue (relayed by the [webhook ingress](#the-webhook-ingress); `resource_id` the issue number, GitLab's `iid`; `action` `opened`) | the common delivery metadata (below), `number`, `title` (cut to 256 bytes), `url`, `labels` (the first 100) |
| `hub.forge.issue.edited` | `issue` | An issue's title or body changed (`action` `edited`) | as `hub.forge.issue.opened` |
| `hub.forge.issue.closed` | `issue` | An issue was closed | as `hub.forge.issue.opened` |
| `hub.forge.issue.reopened` | `issue` | An issue was reopened | as `hub.forge.issue.opened` |
| `hub.forge.issue.labeled` | `issue` | A label was put on an issue (a GitLab update that adds several gives one each, sorted by name) | `number`, `label`, `labels` (the issue's labels now, the first 100), `url` |
| `hub.forge.issue.unlabeled` | `issue` | A label was taken off an issue (on GitLab, before the `labeled` events of the same update) | as `hub.forge.issue.labeled` |
| `hub.forge.comment.created` | `comment` | A new comment on an issue, pull request or merge request (`resource_id` its number). The comment's text is not included | `number`, `comment_url`, `pull_request` (true for a pull or merge request) |
| `hub.forge.pull_request.opened` | `pull_request` | A pull or merge request was opened | `number`, `url`, `head`, `base` (branch names), `merged` (bool) |
| `hub.forge.pull_request.closed` | `pull_request` | One was closed without being merged | as `hub.forge.pull_request.opened` |
| `hub.forge.pull_request.merged` | `pull_request` | One was merged (never also `closed`) | as `hub.forge.pull_request.opened` |
| `hub.forge.pull_request.reopened` | `pull_request` | One was reopened | as `hub.forge.pull_request.opened` |
| `hub.forge.pull_request.synchronize` | `pull_request` | Commits were pushed to its branch | as `hub.forge.pull_request.opened` |
| `hub.forge.push` | `repository` | Commits or a tag were pushed (`resource_id` the ref) | `ref`, `before`, `after` |
| `hub.forge.webhook.set` | `webhook` | `PUT .../forge/webhook` stored the secret (`resource_id` the slug) | `generated`: true when the hub generated it. Never the secret |
| `hub.forge.webhook.unset` | `webhook` | `DELETE .../forge/webhook`, also when there was no secret | `existed` |
| `hub.branch.push` | `branch` | A push attempt of `POST .../branches/:name/push`, successful or not, also when the caller went away; never for a refusal or a `500` (`resource_id` the branch, `action` `push`) | `branch`, `sha`, `force`, `ok`, `remote` (`origin`), `remote_url` (without userinfo), `result` when `ok`, `error_type` when not. Never a credential or the forge's message |

**Forge write events** (`hub.forge.issue.create` to
`hub.forge.pull_request.comment`) say who asked the hub to write. One is
recorded for each write that sent at least one request to the forge,
successful or not, before the answer is written; a request the hub refuses on
its own records nothing. Common metadata: `target` (`origin` or `upstream`),
`ok`, `error_type` (only when `ok` is false: the type the hub answered with),
`number` when known, and `url` (the issue, pull request or comment URL the
forge returned, without userinfo). They never hold a credential, a title, a
body, a comment or a message from the forge.

**Delivery events** (`hub.forge.issue.opened` to `hub.forge.push`, thirteen of
them, named in the past tense) are what a forge reported about the repository.
They are relayed by the [webhook ingress](#the-webhook-ingress), one for each
event the delivery maps to and in that order, after the forge has been
answered. The actor is the forge (`actor_type` `forge`, `actor_id` empty).
Common metadata: `forge` (`github` or `gitlab`), `target` (`origin` or
`upstream`), `delivery_id` and `actor` (the forge login; `""` when the payload
has none), plus the keys in the table and no other payload field; URLs have
their userinfo removed. Read them with `GET /api/v1/events?workspace=<slug>` or
`GET /api/v1/audit?workspace=<slug>`.

**Other hub events.** These are recorded too:

| Event Type | Resource Type | When Emitted | Metadata |
|------------|---------------|--------------|----------|
| `hub.workspace.create` | `workspace` | `POST /workspaces` | `git_url`, `branch` |
| `hub.workspace.update` | `workspace` | `PATCH /workspaces/:slug` | `fields` |
| `hub.workspace.archive` | `workspace` | `POST .../archive` | `head_sha` (when known) |
| `hub.workspace.reactivate` | `workspace` | `POST .../reactivate` | none |
| `hub.workspace.sync` | `workspace` | A standard sync succeeded | `result` (`up_to_date`, `fast_forward` or `reset_to_upstream`) |
| `hub.git.push` | `workspace` | A push to the git server had at least one accepted ref | `head_sha`, `refs_updated` |
| `hub.session.force_closed` | `session` | Archiving or deleting the workspace ended an active agent session (`action` `terminate`, actor type `system`) | `session_id`, `reason` |
| `hub.secret.create`, `.update`, `.delete` | `secret` | A secret was created, updated or deleted | `scope` (`user`, `org` or `workspace`), `key`. Never the value |
| `hub.variable.create`, `.update`, `.delete` | `variable` | A variable was created, updated or deleted | `scope`, `key` |
| `hub.patch.delete` | `patch` | `DELETE .../patches/:id` | `branch_name` |
| `hub.rebuild.enqueue` | `patch` | `POST .../rebuild` queued a rebuild | `job_id`, `patch_count` |
| `hub.rebuild.complete` | `patch` | A rebuild completed | `patches_applied` |
| `hub.rebuild.fail` | `patch` | A rebuild failed | `reason` |
| `hub.rebuild.push_failed` | `patch` | The push of the rebuilt integration branch to `origin` failed | `integration_branch`, `reason` |
| `hub.merge.enqueue` | `merge` | `POST .../merges` queued a merge job | `target_branch`, `source_ref`, `job_id` |
| `hub.merge.complete` | `merge` | A merge job completed | `base_sha`, `merged_sha` |
| `hub.merge.fail` | `merge` | A merge job failed | `reason` |

---

## Audit Ingestion Endpoints

Agents report what happened during a run to the hub: audit events, session
outcomes, tool calls and errors, conversation traces and a postmortem. All
routes live under `/api/v1/workspaces/:slug/runs/:run_id/`.

**Access.** The `POST` routes need `audit:write` and are for the workspace's
**owner** and admin tokens only. They check, in order: the workspace exists
(`404 workspace not found`; also for a workspace other than a bound token's
own), it is not archived (`409`, `error_type: workspace_archived`, answered
before ownership is looked at), and the caller owns it: anyone else, a member
of the workspace's organization or a stranger, gets `403`,
`error_type: workspace_access_denied`, message `token owner does not have write
access to this workspace` (not `404`). A refused request stores nothing. The
`GET` routes need `audit:read` and read access: the owner, a member and an
admin token read them whatever the workspace's status; anyone else gets
`404 workspace not found`. The `run_id`, `node_id` and `postmortem_not_found`
checks come after the access check.

**Common rules:**

- `run_id` has the form `YYYYMMDD_HHMMSS_` and six lowercase hex characters
  (e.g. `20260704_143022_a1b2c3`); anything else is `400`.
- Records are idempotent by `id`: sending a record whose `id` exists answers
  `200` instead of `201` and changes nothing.
- Client timestamps (`timestamp`, `started_at`, `completed_at`) must be RFC
  3339; malformed values are `400` (per item in the batch routes).
- The `GET` routes page with `order` (`asc` or `desc`, default `desc`),
  `limit` (default 50, at most 500), `cursor` and `since`/`until` (RFC 3339,
  `since` inclusive, `until` exclusive), and answer
  `{<items>: [...], "next_cursor": ..., "has_more": ...}`.

### POST /api/v1/workspaces/:slug/runs/:run_id/events

Ingest one audit event.

**Access:** `audit:write`; the workspace's owner.

**Request:**

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `id` | no | string | Client-chosen UUID; generated when omitted |
| `event_type` | yes | string | Event type |
| `severity` | no | string | `info`, `warning`, `error` or `critical`. Defaults to `error` for `session.fail`, `warning` for `run.limit_reached`, `git.conflict`, `harvest.empty` and `review.parse_failure`, and `info` otherwise |
| `node_id` | no | string | Task graph node |
| `session_id` | no | string | Session |
| `timestamp` | no | string | Defaults to now |
| `payload` | no | object | Arbitrary JSON |

**Response:** `201 Created` with `{id, run_id, event_type, severity,
created_at}`; `200 OK` for a duplicate `id`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `event_type` missing; an invalid `run_id`, or a `run_id` in the body that differs from the URL's; an invalid `severity` or `timestamp` |
| 403 | `workspace_access_denied` |
| 409 | `workspace_archived` |

### GET /api/v1/workspaces/:slug/runs/:run_id/events

List a run's audit events.

**Access:** `audit:read`; read access.

**Request:** filters `event_type`, `severity`, `node_id`, `session_id`, and the
[common paging parameters](#audit-ingestion-endpoints).

**Response:** `200 OK`

```json
{
  "events": [...],
  "next_cursor": "string or null",
  "has_more": true
}
```

**Errors:** `400` for an invalid `run_id`, cursor or time.

### POST /api/v1/workspaces/:slug/runs/:run_id/events/batch

Ingest 1 to 1000 audit events in one request.

**Access:** `audit:write`; the workspace's owner.

**Request:** a JSON array of [events](#post-apiv1workspacesslugrunsrun_idevents).

**Response:** `200 OK`; each item is accepted, a duplicate or an error on its
own:

```json
{
  "accepted": 95,
  "duplicates": 3,
  "errors": [
    {"index": 7, "id": "...", "message": "missing event_type"}
  ]
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | An empty array; an invalid `run_id` |
| 413 | More than 1000 items |

### POST /api/v1/workspaces/:slug/runs/:run_id/sessions/outcomes

Ingest one session outcome.

**Access:** `audit:write`; the workspace's owner.

**Request:**

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `id` | no | string | Client-chosen UUID; generated when omitted |
| `session_id` | yes | string | Session |
| `node_id` | no | string | Task graph node |
| `status` | yes | string | Outcome status |
| `timestamp` | no | string | When it happened |
| `duration_ms` | no | integer | Duration in milliseconds, not negative |
| `token_usage` | no | object | Arbitrary JSON |

**Response:** `201 Created` with `{id, run_id, node_id, status, created_at}`;
`200 OK` for a duplicate.

**Errors:** `400` for a missing field or an invalid value; `403`, `409` as for
[events](#post-apiv1workspacesslugrunsrun_idevents).

### GET /api/v1/workspaces/:slug/runs/:run_id/sessions/outcomes

List a run's session outcomes.

**Access:** `audit:read`; read access.

**Request:** filters `session_id`, `status`, `node_id`, and the common paging
parameters.

**Response:** `200 OK` with `{outcomes: [...], next_cursor, has_more}`.

**Errors:** `400` for an invalid `run_id`, cursor or time.

### POST /api/v1/workspaces/:slug/runs/:run_id/tools/calls

Ingest one tool call.

**Access:** `audit:write`; the workspace's owner.

**Request:**

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `id` | no | string | Client-chosen UUID |
| `tool_name` | yes | string | The tool |
| `node_id` | no | string | Task graph node |
| `session_id` | no | string | Session |
| `timestamp` | no | string | When it was called |
| `duration_ms` | no | integer | Duration in milliseconds |
| `input` | no | object | The tool's input |
| `output` | no | object | The tool's output |

**Response:** `201 Created` with `{id, run_id, tool_name, called_at}`; `200 OK`
for a duplicate.

**Errors:** `400` for a missing field or an invalid value; `403`, `409` as for
[events](#post-apiv1workspacesslugrunsrun_idevents).

### GET /api/v1/workspaces/:slug/runs/:run_id/tools/calls

List a run's tool calls.

**Access:** `audit:read`; read access.

**Request:** filters `tool_name`, `session_id`, `node_id`, and the common
paging parameters.

**Response:** `200 OK` with `{calls: [...], next_cursor, has_more}`.

**Errors:** `400` for an invalid `run_id`, cursor or time.

### POST /api/v1/workspaces/:slug/runs/:run_id/tools/errors

Ingest one tool error.

**Access:** `audit:write`; the workspace's owner.

**Request:**

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `id` | no | string | Client-chosen UUID |
| `tool_name` | yes | string | The tool |
| `node_id` | no | string | Task graph node |
| `session_id` | no | string | Session |
| `error_code` | no | string | Machine-readable code |
| `error_msg` | yes | string | Message |
| `timestamp` | no | string | When it failed |

**Response:** `201 Created` with `{id, run_id, tool_name, failed_at}`; `200 OK`
for a duplicate.

**Errors:** `400` for a missing field or an invalid value; `403`, `409` as for
[events](#post-apiv1workspacesslugrunsrun_idevents).

### GET /api/v1/workspaces/:slug/runs/:run_id/tools/errors

List a run's tool errors.

**Access:** `audit:read`; read access.

**Request:** filters `tool_name`, `session_id`, `node_id`, and the common
paging parameters.

**Response:** `200 OK` with `{errors: [...], next_cursor, has_more}`.

**Errors:** `400` for an invalid `run_id`, cursor or time.

### POST /api/v1/workspaces/:slug/runs/:run_id/traces

Ingest one conversation trace event.

**Access:** `audit:write`; the workspace's owner.

**Request:**

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `id` | no | string | Client-chosen UUID |
| `event_type` | yes | string | `session.init`, `assistant.message`, `tool.use`, `tool.error` or `session.result` |
| `node_id` | no | string | Task graph node |
| `session_id` | no | string | Session |
| `sequence` | no | integer | Sequence number |
| `timestamp` | no | string | When it happened |
| `data` | no | object | Arbitrary JSON |

**Response:** `201 Created` with `{id, run_id, event_type, timestamp}`;
`200 OK` for a duplicate.

**Errors:** `400` for a missing field or an invalid value; `403`, `409` as for
[events](#post-apiv1workspacesslugrunsrun_idevents).

### GET /api/v1/workspaces/:slug/runs/:run_id/traces

List a run's trace events.

**Access:** `audit:read`; read access.

**Request:** filters `event_type`, `session_id`, `node_id`, and the common
paging parameters.

**Response:** `200 OK` with `{traces: [...], next_cursor, has_more}`.

**Errors:** `400` for an invalid `run_id`, cursor or time.

### POST /api/v1/workspaces/:slug/runs/:run_id/traces/batch

Ingest 1 to 1000 trace events in one request, as
[`events/batch`](#post-apiv1workspacesslugrunsrun_ideventsbatch) does for
events.

**Access:** `audit:write`; the workspace's owner.

**Response:** `200 OK` with `{accepted, duplicates, errors}`.

**Errors:** as for [`events/batch`](#post-apiv1workspacesslugrunsrun_ideventsbatch).

### POST /api/v1/workspaces/:slug/runs/:run_id/postmortem

Submit the run's postmortem. A run has at most one.

**Access:** `audit:write`; the workspace's owner.

**Request:**

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `schema_version` | no | integer | `1` when given |
| `run_status` | yes | string | `stalled`, `block_limit`, `cost_limit` or `session_limit` |
| `started_at` | yes | string | When the run started |
| `completed_at` | yes | string | When it ended |
| `task_summary` | yes | object | Must contain `total` |
| `cost_summary` | yes | object | Token and cost totals |
| `blocked_tasks` | no | array | Objects with `node_id` and `reason` |
| `session_history` | no | array | Stored as given |

**Response:** `201 Created` with `{run_id, run_status, created_at}`; `200 OK`
when the run already has one.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | A missing field; an invalid `run_status` or time |
| 422 | `unknown_schema_version` |

### GET /api/v1/workspaces/:slug/runs/:run_id/postmortem

Read the run's postmortem.

**Access:** `audit:read`; read access.

**Response:** `200 OK` with `run_id`, `workspace`, `schema_version`,
`run_status`, `started_at`, `completed_at`, `task_summary`, `cost_summary`,
`blocked_tasks`, `session_history` and `ingested_at`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | `postmortem_not_found`: the run has no postmortem |

---

## Audit Query Endpoints

These routes read audit data across sources: one query over hub and agent
events, a transcript rebuilt from a run's traces, and a live stream.

### GET /api/v1/audit

Query hub events and agent events together, newest first.

**Access:** `audit:read`. A user gets the events of every workspace they own or
share an organization with, whatever its status; hub events that belong to no
workspace (user, organization and credential changes) are returned to admin
tokens only.

**Request:** query parameters:

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `source` | string | all | `"hub"` or `"agent"` |
| `workspace` | string | all | One workspace; one the caller may not read is `404 workspace not found` |
| `event_type` | string | all | Exact event type |
| `event_type_prefix` | string | all | Event type prefix (`"hub.workspace"` matches every workspace event) |
| `severity` | string | all | Severity |
| `actor_id`, `actor_type`, `resource_type`, `action` | string | all | Hub events only; giving one leaves agent events out |
| `run_id` | string | all | Agent events only; giving it leaves hub events out |
| `since` | string | | Inclusive lower bound |
| `until` | string | | Exclusive upper bound |
| `limit` | integer | 100 | At most 1000 |
| `cursor` | string | | [Pagination](#conventions) cursor |

**Response:** `200 OK`. Times of both sources are compared as instants, so the
two interleave correctly; `timestamp` is RFC 3339 UTC, with fractional seconds
where present.

```json
{
  "events": [
    {
      "id": "uuid-string",
      "event_type": "hub.workspace.create",
      "source": "hub",
      "timestamp": "2026-09-01T12:00:00Z",
      "severity": "info",
      "workspace": "my-workspace",
      "actor_id": "user-uuid",
      "actor_type": "api_key",
      "resource_type": "workspace",
      "resource_id": "my-workspace",
      "action": "create",
      "run_id": null,
      "node_id": null,
      "session_id": null,
      "archetype": null
    }
  ],
  "next_cursor": "base64-encoded-cursor-or-null",
  "has_more": false
}
```

| Field | Type | Description |
|-------|------|-------------|
| `events[].id` | string | The event's id |
| `events[].event_type` | string | Dot-separated type (e.g. `hub.workspace.create`; see [Hub Audit Events](#hub-audit-events)) |
| `events[].source` | string | `"hub"` or `"agent"` |
| `events[].timestamp` | string | When it happened |
| `events[].severity` | string | Severity |
| `events[].workspace` | string | Workspace slug |
| `events[].actor_id`, `.actor_type`, `.resource_type`, `.resource_id`, `.action` | string or null | Hub events only |
| `events[].run_id`, `.node_id`, `.session_id`, `.archetype` | string or null | Agent events only |
| `next_cursor` | string or null | Cursor of the next page |
| `has_more` | boolean | Whether more events exist |

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | An invalid cursor or time; `since` not before `until` |
| 404 | `workspace` names a workspace the caller may not read |

### GET /api/v1/workspaces/:slug/runs/:run_id/transcript

Rebuild the conversation of one node of a run from its trace events.

**Access:** `audit:read`; read access.

**Request:** query parameter `node_id` (required).

**Response:** `200 OK`

```json
{
  "run_id": "run-uuid",
  "node_id": "node-uuid",
  "messages": [
    {
      "role": "system",
      "content": "System initialization message",
      "tool_name": null,
      "timestamp": "2026-09-01T12:00:00Z"
    },
    {
      "role": "tool_use",
      "content": "Tool output",
      "tool_name": "bash",
      "timestamp": "2026-09-01T12:00:02Z"
    }
  ]
}
```

Trace event types become roles: `session.init` is `system`,
`assistant.message` `assistant`, `tool.use` `tool_use` and `tool.error`
`tool_error`; other types are left out.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | `node_id` missing |

### GET /api/v1/events

Stream audit events live as Server-Sent Events (`text/event-stream`).

**Access:** `audit:read`. A user receives the events of the workspaces they own
or share an organization with; that set is fixed when the stream opens, so
joining or leaving an organization takes effect on the next connection.

**Request:** query parameters:

| Parameter | Type | Description |
|-----------|------|-------------|
| `workspace` | string | Only this workspace; one the caller may not read is `404 workspace not found`, before the stream starts |
| `category` | string | `hub` or `agent` |
| `run_id` | string | Accepted but ignored: streamed hub events carry no run id |

**Response:** `200 OK`, `Content-Type: text/event-stream`, with two kinds of
frame:

```
event: heartbeat
data: {"timestamp":"2026-09-01T12:00:00Z"}
```

```
event: audit_event
data: {"id":"uuid","event_type":"hub.workspace.create","timestamp":"...","workspace":"..."}
```

A heartbeat is sent at once and then every 30 seconds. A connection with no
reads for 60 seconds is closed. Each client has a buffer of 256 events; when it
is full the oldest event is dropped. The number of streams is limited by
`AF_SSE_MAX_CONNECTIONS` (default 100; see
[configuration.md](configuration.md)).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | `workspace` names a workspace the caller may not read |
| 503 | The limit of concurrent streams is reached |

---

## Identity and Access Endpoints (apikit-provided)

Users, API keys, PATs and organizations are managed by the
[apikit](https://github.com/txsvc/apikit) library the hub is built on. Its
routes live under the same mount point as the hub's and use the same
credentials and error envelope; they use apikit's own scopes. A
[bound token](#workspace-bound-tokens) can use none of them.

### Login

#### GET /api/v1/auth/providers

List the configured OAuth providers and the authorize URL of each.

**Access:** public. Cached publicly for five minutes.

**Response:** `200 OK`

```json
[
  {
    "name": "github",
    "authorize_url": "https://github.com/login/oauth/authorize?client_id=..."
  }
]
```

#### POST /api/v1/auth/callback

Exchange an OAuth authorization code for the user and a new API key. This is
the **only** way to get an API key; there is no password login.

**Access:** public.

**Request:**

```json
{
  "provider": "github",
  "code": "authorization-code-from-provider",
  "redirect_uri": "http://localhost:8080/callback",
  "expires": 90
}
```

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `provider` | yes | string | A provider from `GET /api/v1/auth/providers` |
| `code` | yes | string | The provider's authorization code |
| `redirect_uri` | no | string | Must be on the provider's redirect allowlist |
| `expires` | no | integer | Key lifetime in days: 0 (never), 30, 60 or 90. Default 90 |

**Behaviour:** the user is created or updated, **every** existing API key of
the user is revoked, and a new key is issued, all at once. Automation that holds
an older key of the same user stops working after a login. A new user also gets
a personal organization.

**Response:** `200 OK`. The key is returned only here.

```json
{
  "user": {
    "id": "uuid-string",
    "username": "alice",
    "email": "alice@example.com",
    "full_name": "Alice Example",
    "status": "active",
    "role": "user",
    "provider": "github",
    "provider_id": "12345",
    "created_at": "2024-01-01T00:00:00Z",
    "updated_at": "2024-01-01T00:00:00Z"
  },
  "api_key": {
    "key": "af_<key_id>_<secret>",
    "key_id": "<key_id>",
    "expires_at": "2024-04-01T00:00:00Z"
  }
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | Missing or invalid fields; `redirect_uri` not allowed |
| 401 | The code is invalid or expired |
| 403 | The user is blocked |
| 409 | The user exists under a conflicting identity |

### User

#### GET /api/v1/user

Read the caller's profile.

**Access:** `users:read`.

**Response:** `200 OK` with the user (fields as in the
[callback](#post-apiv1authcallback)).

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | The user does not exist |

#### PATCH /api/v1/user

Change the caller's `full_name`, the only mutable field.

**Access:** `users:read`.

**Request:** `{"full_name": "Alice Example"}`; `full_name` is required.

**Response:** `200 OK` with the updated user.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | An invalid body; `full_name` missing or null |
| 404 | The user does not exist |

### Keys

API keys are created only by the [login](#post-apiv1authcallback); these routes
list, rotate and revoke them.

#### GET /api/v1/user/keys

List the caller's API keys: metadata only, never a secret or its hash.
Supports conditional `GET` with `ETag`.

**Access:** `keys:read`.

**Response:** `200 OK`

```json
[
  {
    "key_id": "<key_id>",
    "created_at": "2024-01-01T00:00:00Z",
    "expires_at": "2024-04-01T00:00:00Z",
    "revoked_at": null
  }
]
```

`304 Not Modified` when the `ETag` matches.

#### POST /api/v1/user/keys/:key_id/refresh

Rotate an API key's secret. `key_id` stays; the expiry is computed again from
the key's lifetime.

**Access:** an API key only; PATs are refused (`401`).

**Response:** `200 OK`. The key is returned only here.

```json
{
  "key": "af_<key_id>_<new-secret>",
  "key_id": "<key_id>",
  "expires_at": "2024-07-01T00:00:00Z"
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The key is revoked or expired |
| 401 | Not an API key |
| 404 | The key does not exist or is not the caller's |

#### DELETE /api/v1/user/keys/:key_id

Revoke an API key. A key may revoke itself; the request still completes.

**Access:** `keys:manage`.

**Response:** `204 No Content`.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | The key does not exist or is not the caller's |

### Tokens

#### GET /api/v1/user/tokens

List the caller's PATs with their permissions.

**Access:** `tokens:read`.

**Response:** `200 OK`

```json
[
  {
    "token_id": "<token_id>",
    "name": "CI token",
    "permissions": ["workspaces:read", "workspaces:write"],
    "expires_at": "2024-04-01T00:00:00Z",
    "created_at": "2024-01-01T00:00:00Z",
    "revoked_at": null
  }
]
```

#### POST /api/v1/user/tokens

Create a PAT. To bind it to one workspace, use
[`POST /api/v1/workspaces/:slug/tokens`](#post-apiv1workspacesslugtokens)
instead.

**Access:** `tokens:manage`. A PAT may grant only permissions it holds itself;
an API key may grant any registered permission.

**Request:**

```json
{
  "name": "CI token",
  "permissions": ["workspaces:read", "workspaces:write"],
  "expires": 90
}
```

| Field | Required | Type | Description |
|-------|----------|------|-------------|
| `name` | yes | string | Label; at most 255 characters |
| `permissions` | yes | string[] | [Scopes](#permission-scopes) to grant |
| `expires` | no | integer | Lifetime in days: 0 (never), 30, 60 or 90 |

**Response:** `201 Created`. The token is returned only here.

```json
{
  "token_id": "<token_id>",
  "name": "CI token",
  "token": "af_pat_<token_id>_<secret>",
  "permissions": ["workspaces:read", "workspaces:write"],
  "expires_at": "2024-04-01T00:00:00Z",
  "created_at": "2024-01-01T00:00:00Z"
}
```

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | An invalid body; `name` missing or too long; `permissions` empty, malformed or unknown (old scope names included); a bad `expires` |
| 403 | No `tokens:manage`; a PAT granting a permission it does not hold |

#### GET /api/v1/user/tokens/:token_id

Read one of the caller's PATs.

**Access:** `tokens:read`.

**Response:** `200 OK` with the token's metadata.

**Errors:**

| Status | Condition |
|--------|-----------|
| 404 | The token does not exist or is not the caller's |

#### DELETE /api/v1/user/tokens/:token_id

Revoke a PAT.

**Access:** `tokens:manage`.

**Response:** `200 OK` with the revoked token's metadata (not `204`).

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | The token is already revoked |
| 404 | The token does not exist or is not the caller's |

#### PUT / PATCH / DELETE /api/v1/user/tokens/:token_id/permissions

Change a PAT's permissions: `PUT` replaces the list, `PATCH` adds to it,
`DELETE` removes from it.

**Access:** `tokens:write` or `tokens:manage`. A PAT may grant only
permissions it holds.

**Request:** `{"permissions": ["workspaces:read"]}`. `permissions` must be
present and not null. On `PUT` an empty array removes every permission and
revokes the token; on `PATCH` and `DELETE` it is refused.

**Response:** `200 OK` with the token's metadata after the change.

**Errors:**

| Status | Condition |
|--------|-----------|
| 400 | An invalid body; `permissions` absent, or empty on `PATCH`/`DELETE`; the token is revoked |
| 403 | No scope; a PAT granting a permission it does not hold |
| 404 | The token does not exist or is not the caller's |

### Orgs

#### GET /api/v1/user/orgs

List the organizations the caller belongs to.

**Access:** `orgs:read`.

**Response:** `200 OK`

```json
[
  {
    "id": "org-id",
    "name": "My Organization",
    "slug": "my-org",
    "url": "https://example.com",
    "owner_id": "uuid-string",
    "status": "active",
    "created_at": "2024-01-01T00:00:00Z",
    "updated_at": "2024-01-01T00:00:00Z"
  }
]
```

#### GET /api/v1/orgs/:id

Read an organization, by **id** (the hub's organization
[secrets](#secrets-endpoints) and [variables](#variables-endpoints) routes take
its slug). Supports conditional `GET` with `ETag`.

**Access:** `orgs:read`; a member of the organization, or an admin.

**Response:** `200 OK` with the organization; `304 Not Modified` when the
`ETag` matches.

**Errors:**

| Status | Condition |
|--------|-----------|
| 403 | Not a member |
| 404 | The organization does not exist |

### Administration

These routes need an admin token or an API key of a user with the admin role
(there is no `/admin` prefix). Every one answers `401` without a credential and
`403` for a non-admin. An admin-role API key is an admin here only; towards
workspaces and the git server only an admin token has access to everything.

#### Users

| Endpoint | Description |
|----------|-------------|
| `POST /api/v1/users` | Create a user. Body: `{username, email, provider, provider_id}`. `201`, or `409` when the username or provider identity exists |
| `GET /api/v1/users` | List all users |
| `GET /api/v1/users/:id` | Read any user. Supports `ETag` (`304`) |
| `PATCH /api/v1/users/:id` | Change any user. Body: `{full_name}`, required |
| `DELETE /api/v1/users/:id` | Delete a user; `204`, or `409` for the last active admin |
| `POST /api/v1/users/:id/promote` | Make a user an admin. Idempotent |
| `POST /api/v1/users/:id/demote` | Take the admin role away; `409` for the last admin |
| `POST /api/v1/users/:id/block` | Block a user: their API keys stop working without being revoked |
| `POST /api/v1/users/:id/unblock` | Unblock a user: every key that is not revoked or expired works again |
| `GET /api/v1/users/:id/keys` | List any user's API keys (entries include `user_id`) |
| `DELETE /api/v1/users/:id/keys/:key_id` | Revoke any user's API key; `204` |
| `GET /api/v1/users/:id/tokens` | List any user's PATs (entries include `user_id`) |
| `DELETE /api/v1/users/:id/tokens/:token_id` | Revoke any user's PAT; `204` |

#### Organizations

| Endpoint | Description |
|----------|-------------|
| `POST /api/v1/orgs` | Create an organization. Body: `{name, slug, url, owner_id}`. `201`; `409` when the name or slug exists; `400` for an invalid slug |
| `GET /api/v1/orgs` | List all organizations |
| `PATCH /api/v1/orgs/:id` | Change an organization. Body: `{name, url}`, at least one; `409` on a name collision |
| `DELETE /api/v1/orgs/:id` | Delete an organization; `204` |
| `POST /api/v1/orgs/:id/block` | Block an organization |
| `POST /api/v1/orgs/:id/unblock` | Unblock an organization |
| `GET /api/v1/orgs/:id/members` | List members |
| `PUT /api/v1/orgs/:id/members/:user_id` | Add a member; returns the membership |
| `DELETE /api/v1/orgs/:id/members/:user_id` | Remove a member; `204` |

A membership is `{org_id, user_id, username, email, role, created_at}`.

---

## Server Endpoints

These routes are served at the server root, outside `/api/v1`, and need no
credential.

### GET /healthz

Liveness probe: the process is up.

**Response:** `200 OK` with `{"status": "ok"}`.

### GET /readyz

Readiness probe: the hub's database answers.

**Response:** `200 OK` with `{"status": "ready"}`; `503` with the error envelope
when the database is unreachable.

### GET /version

Build information and the API mount point. Cached publicly.

**Response:** `200 OK`

```json
{
  "go_version": "1.27.0",
  "build_time": "2026-09-01T00:00:00Z",
  "commit": "abc1234",
  "mount_point": "/api/v1"
}
```

### GET /metrics

Operational metrics in the Prometheus text format, for a scraper.

**Response:** `200 OK`, `Content-Type: text/plain; version=0.0.4`:

| Metric | Type | Labels | Description |
|--------|------|--------|-------------|
| `afhub_http_requests_total` | Counter | method, path, status | HTTP requests |
| `afhub_http_request_duration_seconds` | Histogram | method, path | Request latency (buckets 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10) |
| `afhub_agent_sessions_active` | Gauge | workspace | Active agent sessions |
| `afhub_agent_tokens_total` | Counter | workspace, model, direction | Token usage |
| `afhub_audit_events_total` | Counter | source, event_type | Audit events ingested |
| `afhub_audit_table_rows` | Gauge | table | Rows per audit table (sampled hourly) |
| `afhub_retention_errors_total` | Counter | step | Failed retention steps |
| `afhub_retention_last_run_timestamp_seconds` | Gauge | | Unix time of the last successful retention run |
| `afhub_sse_connections` | Gauge | | Open [event streams](#get-apiv1events) |
| `afhub_jobqueue_depth` | Gauge | type, status | Registered, but nothing sets it yet: always 0 |
| `afhub_sandboxes_active` | Gauge | workspace | Leases in state `active`, per workspace. Present from start with the placeholder `workspace=""`, which is also what remains while no lease is `active` |
| `afhub_sandbox_creates_total` | Counter | result | Sandbox creates the gateway accepted for a `leased` lease, by `result` (`created` or `failed`); a create refused before that is not counted |
| `afhub_sandbox_lease_seconds` | Histogram | | Seconds from issue to end of a lease that became `destroyed`, whatever the reason (buckets 60, 300, 600, 1200, 1800, 3600, 7200, 14400, 28800); a lease that became `expired` is not counted |

The `path` label is the route template (e.g. `/api/v1/sessions/:id`), not the
requested URI, to bound its cardinality.
