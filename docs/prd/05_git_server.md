[Parley PRD](00_overview.md) · §5

## 5. Git server

### 5.1 Purpose and scope

The hub serves every workspace and every task fork as a git remote over smart HTTP, on the same port as the REST API. Stock `git` clients clone, fetch and push with their hub credentials. The git server authenticates with HTTP Basic, applies the same access rule and scopes as the REST API, enforces the push rules (hub-owned refs, protected branches, fork tracking branches, carry-patch push control, path-scoped tokens), and turns accepted pushes into changes and change versions (§7).

In scope: the three smart-HTTP routes, authentication and authorization on them, protocol support, push rules, the push-to-change trigger and its client-visible output, push events, and the `parley credential-helper` command.

Out of scope: SSH transport and the dumb HTTP protocol (not offered); change semantics (§7); landing and policy (§8); the carry-patch policy behind push control and the rebuild that follows a patch push (§10); the source control API, which reads and writes the same repositories without a git client (§6).

### 5.2 User stories

**GIT-US-01** As a Developer, I want to clone a workspace with `git clone <external_url>/git/<org>/<slug>.git` and my API key, so that I can work with ordinary git tooling.
- A task fork is cloned the same way, with the fork's slug.

**GIT-US-02** As an Agent, I want to fetch only what I need (a shallow history, no blobs until I read them), so that starting work on a large repository is fast.

**GIT-US-03** As an Agent with a PAT, I want my push to open a change, or to add a version to the change my branch already belongs to, so that I do not need a second call.
- `git push -o parley.change` opens a change; in a task fork with `auto_change` the option is not needed.
- The push output names the change and the version.

**GIT-US-04** As an Org member with read-only access, I want to fetch my org's workspaces. A push is refused with a clear message; I contribute through a task fork (§4).

**GIT-US-05** As a Workspace owner, I want direct pushes to protected branches refused for agents and org members, so that their work reaches those branches only by landing (§8). I can still fast-forward a protected branch myself, and I can turn that off.
- A workspace-bound or path-scoped token is refused on a protected branch even when it belongs to the owner.
- Nobody can force-push or delete a protected branch.

**GIT-US-06** As an Operator, I want a path-scoped token to be unable to push commits that touch files outside its paths, so that an agent's write access is bounded.

**GIT-US-07** As an Agent, I want to force-push my own unprotected branch after a rebase, and to fetch earlier versions of any change by ref.

**GIT-US-08** As a Developer, I want `git` to obtain my hub credential through `parley credential-helper`, so that plain hub URLs work and the credential is never sent to another host.

**GIT-US-09** As a consumer of the event stream, I want one event per updated ref, linked to the session that pushed, so that I can follow what agents do.

### 5.3 Concepts and data model

**Git URL.** `<external_url>/git/<org-slug>/<workspace-slug>.git`. `<org-slug>` is the slug of the organization the workspace belongs to. A task fork inherits its parent's org and is served at the same shape with its own slug (`…/git/acme/demo--k3x9w2ab.git`). The workspace object's `hub_url` carries this URL; it is null when `[server].external_url` is empty or the workspace has no organization. A workspace without an organization has no git URL.

**Repository.** The hub's copy is a bare repository. There is no worktree and no checked-out branch. `HEAD` is a symbolic ref to the workspace's default branch, so a clone checks that branch out. The repository is served while the workspace status is `ready`.

**Credential.** The Basic password is a PAT, an API key or the admin token (§3). Bearer tokens are not accepted on git routes. Only PATs are scope-checked: `git:read` for fetch, `git:write` for push; `git:write` satisfies a read check.

**Access.** The workspace access rule of §3 applies unchanged:

| Caller | Fetch | Push |
|---|---|---|
| Admin token | yes | yes |
| Workspace owner | yes | yes |
| Org member (not owner) | yes | no (`403`) |
| Anyone else | `404` | `404` |

A PAT bound to a workspace is valid only on that workspace and on the forks it created (§4). On any other repository it is answered like a stranger, and the refusal is reported as `parley.token.bound_refused` with `surface: "git"` (§3).

**Hub-owned refs.** Everything under `refs/parley/` is written only by the hub. Three families are defined elsewhere: `refs/parley/changes/<number>/<version>` (every version of every change, §7), `refs/parley/replaced/<name>` (backups of patch branch tips the hub replaced, §10) and `refs/parley/sandboxes/<sandbox id>/<branch>` (checkpoints of sandboxes, §13). Hub-owned refs are listed and fetchable by name. A default clone does not fetch them, because git's default refspec covers only branches and tags.

**Ref update.** One command of a push: `{ref, before, after}`. `before` is the all-zero id for a create, `after` is the all-zero id for a delete. An update is **forced** when it is neither a create nor a delete and `before` is not an ancestor of `after`.

The git server has no entities of its own. It reads workspaces, tokens, settings and patches, and it writes objects, refs, the workspace's `head_sha` (§4), changes and versions (§7) and events.

### 5.4 Functional requirements

#### Routing, authentication and authorization

- **GIT-FR-001** Exactly three routes exist, outside `/api/v1`: `GET /git/:org/:slug.git/info/refs`, `POST /git/:org/:slug.git/git-upload-pack` and `POST /git/:org/:slug.git/git-receive-pack`.
- **GIT-FR-002** The REST body size limit (`[server].max_body_size`) does not apply to git routes. `[git].max_push_size` applies instead.
- **GIT-FR-003** Checks run in the order of §3: authenticate (`401`; `403` for a blocked user) → rate limit (`429`) → binding guard for a bound PAT (`404`) → PAT scope (`403`) → repository resolution and access (`404`, or `403` for a read-only member on a push route) → request validation (`400`) → workspace state (`409`) → act.
- **GIT-FR-004** A request with missing, malformed or wrong credentials is answered `401` with `WWW-Authenticate: Basic realm="parley"`, so that git can ask its credential helper. A valid credential of a blocked user is answered `403` with the body `user is blocked`, as §3 requires. The Basic username is never consulted.
- **GIT-FR-005** Need is derived from the route: `git-receive-pack`, and `info/refs` with `service=git-receive-pack`, need write and `git:write`. Everything else needs read and `git:read`.
- **GIT-FR-006** The repository resolves only when the slug names a workspace, the workspace has an organization, and `:org` is that organization's slug. A path segment that does not end in `.git`, an unknown slug, a wrong org and a caller without read access all get the same answer: `404 repository not found`. The answer does not depend on the workspace's status, so a workspace that is not ready does not reveal its existence to callers without access.
- **GIT-FR-007** A caller with read access who addresses a workspace that is not `ready` gets `409` with the message `repository not available: workspace is <status>`. A push to a task fork whose parent is `archived` gets `409` with the message `repository is read-only: parent workspace is archived` (§4, `parent_archived`); fetches from that fork keep working. A fork whose parent is in exclusive maintenance gets `409` with `repository not available: parent workspace is busy`.
- **GIT-FR-008** Membership and token bindings are read on every request. A removed member is a stranger on the next request.
- **GIT-FR-009** Errors before the git exchange starts are answered with the HTTP status and a one-line `text/plain` body, which git prints as `remote: <message>`. The statuses and messages are listed in §5.5. Errors after the exchange has started are in-band: `ERR` packets on fetch, the status report and `remote:` lines on push, with HTTP `200`.

#### Protocol

- **GIT-FR-010** Fetch supports git protocol version 2 when the client asks for it with the `Git-Protocol: version=2` header, and falls back to version 0 otherwise. Push uses the version 0 exchange, as git does.
- **GIT-FR-011** Partial clone MUST work. The filters `blob:none`, `blob:limit=<n>` and `tree:<n>` are accepted; a client can later fetch the omitted objects on demand. Other filter kinds may be refused with an in-band error.
- **GIT-FR-012** Shallow fetch MUST work: `--depth`, `--deepen`, `--shallow-since`, `--shallow-exclude` and `--unshallow`.
- **GIT-FR-013** Incremental fetch negotiates haves and wants and sends only missing objects. Gzip-compressed request bodies are accepted. Progress is streamed over the side band.
- **GIT-FR-014** Every ref is advertised, including `refs/parley/**`. Under version 2 the client's ref-prefix filter keeps the listing small. A client may also request by object id any commit that is reachable from an advertised ref.
- **GIT-FR-015** In a task fork, tracking branches are advertised at the parent's current heads (§4). The fork also advertises, read-only, the version refs `refs/parley/changes/<number>/<version>` of the changes whose source is the fork, under the same names as in the parent, so that the holder of a fork token can fetch every version of its own changes (§7).
- **GIT-FR-016** The push advertisement offers at least `report-status`, `side-band-64k`, `delete-refs`, `ofs-delta`, `atomic` and `push-options`. Thin packs are accepted.

#### Push processing

- **GIT-FR-020** A push is processed in this order: read the commands and push options → validate the push options (GIT-FR-051) → receive the pack into quarantine → evaluate the push rules for every ref (GIT-FR-030) → update all refs together → map branches to changes (GIT-FR-040) → emit events → answer.
- **GIT-FR-021** A push is atomic across refs, whether or not the client asked for `--atomic`. If any ref is refused, no ref is updated, the pushed objects are discarded, no event is emitted and no change is touched. The refused refs carry their own message; every other ref of the push is reported as `not updated: another ref in this push was refused`.
- **GIT-FR-022** Every ref update is a compare-and-swap against the `before` value the client sent. A ref that has moved, a create of an existing ref, and an update or delete of a missing ref are refused with `ref changed since it was read; fetch and try again`. A push never waits for, and is never refused because of, another running operation, except while the workspace is not `ready` (GIT-FR-007).
- **GIT-FR-023** A refusal is reported twice: as the per-ref status (`ng <ref> <message>`, which git shows as `! [remote rejected] <ref> (<message>)`), and as a side-band line `error: <ref>: <message>`, which git shows prefixed with `remote:`. The HTTP status is `200`.
- **GIT-FR-024** A pack that cannot be read, or that exceeds `[git].max_push_size`, fails the push with `unpack <reason>`; nothing changes.
- **GIT-FR-025** A push with no commands is a successful no-op: no event, no change activity.
- **GIT-FR-026** Any ref name outside `refs/parley/` may be pushed: branches, tags, notes and other namespaces. The push rules below decide.
- **GIT-FR-027** When an accepted push moves the default branch, the workspace's `head_sha` (§4) is updated before the push is answered.

#### Push rules

- **GIT-FR-030** For each ref update the rules are evaluated in the order of the table. The first rule that refuses decides the message. Rules are evaluated for all refs before any ref is written or any side effect happens. `<branch>` is the ref name without `refs/heads/`.

| # | Rule | Refused updates | Message |
|---|---|---|---|
| 1 | **Hub-owned refs.** `refs/parley/**` is never client-writable, for any credential including the admin token. | create, update, delete | `refs/parley/ is maintained by the hub and cannot be pushed to` |
| 2 | **Protected branches.** A branch matched by the workspace setting `protected_branches` (§4, §8; default: the default branch) moves only by landing, by sync from origin, or by a direct push under the owner exemption (GIT-FR-034). | create, forced update and delete, for every credential; fast-forward update, unless the owner exemption applies | Forced update or delete: `<branch> is protected and cannot be force-pushed or deleted`. Otherwise: `<branch> is protected; push another branch and open a change (git push -o parley.change)` |
| 2a | **Default branch.** The default branch cannot be deleted, even when it is not protected. | delete | `<branch> is the default branch and cannot be deleted` |
| 3 | **Tracking branches.** In a task fork, every branch inherited from the parent is read-only (§4). | create, update, delete | `<branch> tracks the parent workspace and is read-only in this fork; push a new branch` |
| 4 | **Carry-patch push control.** On a carry-patch workspace the integration branch is hub-owned and registered patch branches follow the workspace's patch settings (GIT-FR-031). | see GIT-FR-031 | see GIT-FR-031 |
| 5 | **Path-scoped tokens.** If the pushing PAT has `paths` (§3, §8), every file changed by the pushed commits must match one of its globs (GIT-FR-032). | the whole push | `path <path> is outside this token's allowed paths` |
| 6 | **Everything else is allowed.** Creates, fast-forward updates, non-fast-forward updates and deletes of unprotected refs are accepted. Agents rewrite their own branches. | none | n/a |

- **GIT-FR-031** Carry-patch push control (policy in §10):

| Ref | Workspace settings | Result |
|---|---|---|
| Integration branch | any | Refused: `integration branch <branch> is maintained by the hub; push a patch branch and rebuild instead` |
| Registered patch branch (patch not deleted) | `patch_branch_source = hub` | Allowed |
| Registered patch branch | `patch_branch_source = origin`, `push_patches_to_origin = false` | Refused: `branch is synced from origin; push to <origin URL without credentials> instead` |
| Registered patch branch, delete | `patch_branch_source = origin`, `push_patches_to_origin = true` | Refused: `branch is synced from origin; delete it on origin instead` |
| Registered patch branch, create or update | `patch_branch_source = origin`, `push_patches_to_origin = true` | Forwarded to origin without force, then allowed. Refused with `failed to resolve origin credentials` or `origin rejected push: <reason without credentials>` when forwarding fails |
| Any other ref | any | Rule 5 and 6 apply |

  Forwarding to origin is the only rule with an outside effect. It runs last, only when every ref of the push has passed every other rule, with a 120 s timeout per branch. If a forward fails, the whole push is refused; branches already forwarded in the same push stay on origin and return through the next sync.

- **GIT-FR-032** Path check for a path-scoped token. The checked commits are those reachable from the pushed heads and not reachable from any ref the repository had before the push (in a fork this includes the parent's refs). For each such commit the changed files are its differences against its first parent, or its whole tree for a root commit; a rename counts with both its old and its new path. If any changed path matches none of the token's globs, the whole push is refused and the message names the first offending path in commit order, then path order. Tag and note pushes are checked the same way. Deletes pass. An update that adds no new commits passes too, unless the branch is the source of a `draft` or `open` change or the push opens one: then the files of the resulting version against its base are checked (LAND-FR-080). Path limits never restrict fetch.
- **GIT-FR-033** If a rule cannot be evaluated because of an internal failure, the push is refused with `internal error; try again`. Rules never fail open.
- **GIT-FR-034 (owner exemption).** Rule 2 lets a fast-forward update of a protected branch pass when all of these hold:
  1. The workspace setting `protect.owner_push` (§4, §8) is true. It is the default.
  2. The caller is the workspace's owner or the admin token.
  3. The credential is an API key, the admin token, or a PAT that is not bound to a workspace and has no `paths`. A workspace-bound PAT and a path-scoped PAT are never exempt, whoever their user is. An org member is never exempt.
  4. The update is a fast-forward: the branch exists and its `before` is an ancestor of `after`.

  Such an update is a **direct push**. A create, a forced update and a delete of a protected branch are refused for every credential, including the admin token. The exemption lifts rule 2 only; rules 1, 3 and 4 still apply to the ref. With `protect.owner_push` false, rule 2 refuses every update for every credential.
- **GIT-FR-035** A direct push is recorded: its `parley.git.push` event carries `direct: true` (§5.8). Its commits belong to no change, and commit objects report them with `change: null` (§6). The push opens no change for the protected branch (GIT-FR-040).

#### Push to change mapping

- **GIT-FR-040** After the refs of an accepted push are written, the hub looks at each created or updated branch (`refs/heads/*`; tags, other refs and deletes are skipped), in command order:
  1. If the branch is the source of a change in state `open` or `draft`, a new version is recorded for that change (§7). No option is needed.
  2. Otherwise a change is opened when the push carries `parley.change` or `parley.target`, or when the workspace is a task fork with `auto_change` on (§4).
  3. Otherwise nothing happens. The branch is just a branch.

  A protected branch that moved by a direct push (GIT-FR-034) is skipped at step 2: the push opens no change for it. When the push carries `parley.change` or `parley.target`, the client gets the line `warning: no change for <branch>: branch is protected and was pushed directly`.
- **GIT-FR-041** The change belongs to the workspace of its target branch. For a push to a task fork that is the parent workspace, and the source is the fork's branch. The default target is the fork's base branch (`fork_point.ref`) in a fork, and the workspace's default branch otherwise.
- **GIT-FR-042** The mapping runs before the push is answered, so that its result can be reported. For each branch the client receives one side-band line, shown by git with the `remote:` prefix:

| Outcome | Line |
|---|---|
| New version of an existing change | `change <workspace>#<number> version <n>` |
| Change opened | `change <workspace>#<number> version 1 (opened)` |
| Change opened as draft | `change <workspace>#<number> version 1 (opened, draft)` |
| Change not opened | `warning: no change for <branch>: <reason>` |

  `<workspace>` is the slug of the workspace the change belongs to. The lines carry no URL. Example:

```
$ git push -o parley.change -o parley.title="Fix retry loop" origin fix-retry
remote: change demo#12 version 1 (opened)
To https://hub.example/git/acme/demo.git
 * [new branch]      fix-retry -> fix-retry
```

- **GIT-FR-043** A failure in the mapping never fails the push: the refs stay updated, `parley.git.push` is emitted, and the reason is reported on the `warning:` line. Reasons include: the target branch does not exist, the branch is its own target, the branch has no commits ahead of the target, a merge commit among the commits ahead of the target (`merge_commits_unsupported`, §7), a PAT without `changes:write` asking to open a change, and an empty intent when the workspace setting `changes.require_intent` is true and `parley.draft` was not given (`intent_required`, §7). Recording a version on an existing change needs only `git:write`.

#### Push options

- **GIT-FR-050** The hub understands these options. Options without the `parley.` prefix are ignored.

| Option | Value | Effect |
|---|---|---|
| `parley.change` | none | Open a change for every pushed branch that is not already the source of an open or draft change |
| `parley.target=<branch>` | branch name | Target branch of the changes this push opens. Implies `parley.change` |
| `parley.title=<text>` | 1–200 characters | Title of the changes this push opens. Default: the subject line of the branch's head commit |
| `parley.intent=<text>` | up to 1,024 bytes | Intent of the changes this push opens. Default: the body of the head commit's message, then the `purpose` of the source fork (CHG-FR-004) |
| `parley.draft` | none | Open the changes in state `draft` instead of `open` |
| `parley.session=<id>` | session id (§14) | Session recorded on the push events, on new versions and on opened changes |

- **GIT-FR-051** Options are validated before the pack is received. An invalid option refuses the whole push with `error: invalid push option <name>: <reason>` and nothing is written. Invalid means: an unknown `parley.*` name, the same option given twice, a value on `parley.change` or `parley.draft`, a missing or empty value on the others, a `parley.target` that is not a valid branch name, a value over its length limit, or a `parley.session` that does not name a session the caller may use (§14). A push carries at most 16 options of at most 1,024 bytes each.
- **GIT-FR-052** `parley.target`, `parley.title`, `parley.intent` and `parley.draft` affect only changes opened by this push. They never alter an existing change; for that the client uses the changes API (§7). When a push opens several changes, the options apply to each.
- **GIT-FR-053** `parley.target` naming a branch that does not exist is not a validation error. It is a failure to open the change (GIT-FR-043), because the push itself is still valid.
- **GIT-FR-054** `parley.session` takes effect on every push, whether or not a change is involved.

#### After a push

- **GIT-FR-060** One `parley.git.push` event is emitted per updated ref (§5.8). Event delivery problems never fail a push.
- **GIT-FR-061** On a carry-patch workspace, an accepted update of a registered patch branch triggers the post-push behaviour of §10 (rebuild of the integration branch, optional mirror to origin). It runs in the background and never delays the answer.
- **GIT-FR-062** Every git request is counted in `parley_git_requests_total{service,result}` with `service` ∈ `info-refs`, `upload-pack`, `receive-pack` and `result` ∈ `ok`, `refused` (`401`, `403`, `404`, and a push refused by a rule or an invalid option), `error` (§14).

#### Credential helper

- **GIT-FR-070** `parley credential-helper <verb>` implements git's credential helper protocol. Only `get` produces output. `store`, `erase` and any other verb print nothing and exit 0.
- **GIT-FR-071** The helper takes the endpoint and the credential from the CLI's normal configuration (§3): `config.toml` in `~/.parley` or `PARLEY_CONFIG_DIR`, overridden field by field by `PARLEY_ENDPOINT_URL` and `PARLEY_API_KEY`. If either value is missing or the URL has no host, it prints nothing and exits 0.
- **GIT-FR-072** It reads `key=value` lines from stdin up to the first empty line. It answers only when the request's `protocol` equals the hub URL's scheme and the host and port match (host compared case-insensitively; a missing port means 443 for `https` and 80 for `http`). The credential is never offered to another scheme, host or port. The path is not considered.
- **GIT-FR-073** On a match it prints exactly `protocol=<protocol>`, `host=<host as given>`, `username=x-token-auth` and `password=<credential>`, one per line.

### 5.5 REST API

The three endpoints speak git smart HTTP with HTTP Basic authentication. They do not use the JSON error envelope: a git client reads them, so refusals are `text/plain` with one line of text.

Common refusals, in check order:

| Status | Body | Condition |
|---|---|---|
| `401` + `WWW-Authenticate: Basic realm="parley"` | `authentication required` | No, malformed or non-Basic `Authorization`; unknown, revoked or expired credential |
| `403` | `user is blocked` | Valid credential of a blocked user |
| `429` + `Retry-After` | `rate limit exceeded` | Over the request rate limit (§3) |
| `404` | `repository not found` | Bound PAT on a repository its binding does not cover |
| `403` | `token lacks scope git:read` / `token lacks scope git:write` | PAT without the needed scope |
| `404` | `repository not found` | Unknown slug, no org, wrong org, missing `.git`, caller without read access |
| `403` | `org members have read-only access to this workspace; create a task fork to contribute` | Read-only member on a push route |
| `400` | `invalid or missing service parameter` | `info/refs` without `service=git-upload-pack` or `service=git-receive-pack` |
| `409` | `repository not available: workspace is <status>` | Caller has access, workspace not `ready` |
| `409` | `repository is read-only: parent workspace is archived` | Push route on a task fork whose parent is `archived` |
| `500` | `internal error` | Lookup or repository failure before the exchange starts |

#### `GET /git/:org/:slug.git/info/refs`

- **Purpose:** ref discovery for fetch or push.
- **Auth / scope:** Basic. `service=git-upload-pack`: read access, `git:read`. `service=git-receive-pack`: write access, `git:write`.
- **Query:** `service` (required; the first value counts).
- **Response:** `200`, `Content-Type: application/x-<service>-advertisement`, `Cache-Control: no-cache`. For version 2 fetch, the capability advertisement; otherwise the service line and the ref advertisement (GIT-FR-014, GIT-FR-016).
- **Errors:** common refusals.

#### `POST /git/:org/:slug.git/git-upload-pack`

- **Purpose:** fetch and clone.
- **Auth / scope:** Basic; read access; `git:read`.
- **Request:** a git upload-pack request (version 0 or version 2 command), optionally `Content-Encoding: gzip`.
- **Response:** `200`, `Content-Type: application/x-git-upload-pack-result`, `Cache-Control: no-cache`, streamed.
- **Errors:** common refusals; in-band `ERR <message>` for an empty or invalid request, an invalid gzip body, an unsupported filter, a want that is not reachable, or a timeout.

#### `POST /git/:org/:slug.git/git-receive-pack`

- **Purpose:** push.
- **Auth / scope:** Basic; write access (owner or admin token); `git:write`.
- **Request:** command list, optional push options, pack.
- **Response:** `200`, `Content-Type: application/x-git-receive-pack-result`, with the status report (`unpack ok` or `unpack <reason>`, then `ok <ref>` or `ng <ref> <message>` per ref) and side-band lines for refusals and change mapping.
- **Errors:** common refusals; in-band refusals per GIT-FR-021 to GIT-FR-033 and GIT-FR-051.

### 5.6 CLI

#### `parley credential-helper <get|store|erase>`

- **Purpose:** git credential helper that hands the configured hub credential to git for the hub's own URL. It makes no request to the hub.
- **Arguments:** exactly one, the git credential verb. No flags of its own.
- **Output:** for `get` with a match, the four lines of GIT-FR-073 on stdout; otherwise nothing.
- **Exit codes:** 0 in every handled case, including no match and missing configuration; 2 for a wrong number of arguments.
- **Setup:** `git config --global credential.<hub-url>.helper '!parley credential-helper'`. The credential may be an API key or a PAT.

### 5.7 Background behaviour

- Objects of a refused or failed push are held in quarantine and discarded; they never become reachable or fetchable.
- The carry-patch post-push work (§10) and everything a new change version triggers (restack, mergeability, overlap detection; §7, §9) run after the push is answered. What deleting a change's source branch means is specified in §7.

### 5.8 Events

| Event type | When | Fields |
|---|---|---|
| `parley.git.push` | Once per updated ref of an accepted push, in command order | `resource_type: "ref"`, `resource_id: <full ref name>`, `action: "push"`, `session_id` from `parley.session` (else null); metadata `ref` (full name), `before`, `after` (40-hex; null on the side that does not exist: `before` for a create, `after` for a delete), `created`, `deleted`, `forced` (booleans), `direct` (boolean; true when the update moved a protected branch under the owner exemption, GIT-FR-034), `via` (`git` here; the same event is emitted with `api` for ref updates through the source control API, §6, and with `land` when a land moves the target, §8) |
| `parley.git.fetch` | Once per served upload-pack request that sends a pack, only when the workspace setting `audit_fetches` is true | `resource_type: "workspace"`, `resource_id: <slug>`, `action: "fetch"`; metadata `shallow` (boolean), `filter` (string or null) |

Actor fields follow the credential: `actor_type` is `pat`, `api_key` or `admin_token`. For a push to a fork, `workspace` is the fork's slug. Events of the same push are written together and have consecutive positions among the workspace's events (§14); their `seq` values need not be contiguous. `parley.token.bound_refused` is specified in §3; the change events that follow a push in §7; the rebuild and mirror events in §10.

### 5.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[server].external_url` | string | `""` | Prefix of `hub_url`. Empty means `hub_url` is null; routing does not depend on it |
| `[git].timeout` | duration | `30m` | Upper bound for one fetch or push request |
| `[git].max_push_size` | size | `0` (unlimited) | Largest accepted pack in one push |
| Workspace setting `protected_branches` | list of branch globs | the default branch | Push rule 2 (owned by §4 and §8) |
| Workspace setting `protect.owner_push` | bool | `true` | The owner exemption of push rule 2 (GIT-FR-034; owned by §4 and §8) |
| Workspace setting `audit_fetches` | bool | `false` | Emit `parley.git.fetch` |
| `PARLEY_ENDPOINT_URL`, `PARLEY_API_KEY`, `PARLEY_CONFIG_DIR` | env (CLI side) | | Credential helper inputs |

Fixed values: Basic realm `parley`; forward-to-origin timeout 120 s per branch; at most 16 push options of 1,024 bytes each.

### 5.10 Edge cases and error handling

- A PAT without any git scope gets `403` on every git URL, whether or not the repository exists, because the scope check precedes resolution. This reveals nothing. A bound PAT on a repository outside its binding gets `404` first, because the binding guard precedes the scope check (§3).
- `info/refs?service=git-upload-pack&service=git-receive-pack` is treated as upload-pack.
- An owner who pushes a protected branch with a workspace-bound or path-scoped PAT is refused by rule 2, although the same person's API key would pass. The message is the ordinary rule 2 message.
- A direct push that is not a fast-forward is refused with the forced-update message of rule 2, also for the admin token. To rewrite a protected branch, the owner removes it from `protected_branches` first.
- A direct push races with a land on the same branch like any other mover: both are compare-and-swaps. The push that loses is refused with `ref changed since it was read; fetch and try again`; a land that loses retries on the new head (§8).
- Two concurrent pushes to the same branch: one wins, the other is refused with `ref changed since it was read; fetch and try again`. Pushes to different refs do not interfere.
- A force-push that rebases a branch onto a newer target does not trip the path check for the target's commits: they are already in the repository, so only the agent's own commits are checked.
- A client without `push-options` support cannot send options. In a fork with `auto_change` the push still opens a change; elsewhere the client opens the change through the API (§7).
- `parley.change` on a push that updates only tags or deletes branches has no effect and is not an error.
- A branch whose earlier change has landed or been abandoned is not the source of an open change. A later push with `parley.change`, or in a fork with `auto_change`, opens a new change.
- A client without `report-status` or side-band support gets no messages; the rules are enforced all the same.
- Credential helper: hub URL `https://hub` matches a request for `host=HUB:443`; an `http` request for an `https` hub gets no answer.
