[Parley PRD](00_overview.md) · §9

## 9. Awareness

### 9.1 Purpose and scope

When many agents work on one repository, the expensive conflicts are the ones nobody saw coming: two agents rewrite the same file for an hour and one of them finds out at landing. Awareness makes in-flight work visible early. Before an agent starts, it can ask who else is touching the paths it is about to edit. While it works, it is told when another change starts to overlap its own.

Awareness is advisory. It never refuses a push, a commit or a land. Agents and people decide what to do with what they see: wait, coordinate through the other change, stack on top of it, or carry on and resolve the conflict later.

In scope: the activity query, overlaps between changes, claims, and the events that announce overlaps. Out of scope: a message board or chat between agents; locking of paths; line-level merge prediction.

### 9.2 User stories

**AWR-US-01** As an Agent, I want to ask "who is working on `api/**` right now?" before I start, and get the open changes, their intent and their sessions.

**AWR-US-02** As an Agent, I want to announce what I am about to work on before I have any code, so that others see it.
- A claim expires by itself if I crash.

**AWR-US-03** As an Agent, I want to be told when another change starts touching files my open change touches.

**AWR-US-04** As a Developer, I want to see, for one change, every other open change that touches the same files, to decide the order in which they should land.

**AWR-US-05** As an Agent, I want to know what landed in my paths since I forked, so that I can rebase before I go further.

### 9.3 Concepts and data model

**Overlap.** Two changes overlap when they have the same target branch and at least one touched file in common. Overlap is computed on file paths (both paths of a rename), not on lines. Changes in one stack do not overlap each other.

**Overlap entry.**

| Field | Type | Meaning |
|---|---|---|
| `change` | object | Summary of the other change: `number`, `ref`, `title`, `intent` (first 500 characters), `state`, `author`, `session_id`, `version`, `updated_at`, `mergeable`, `landing.status` |
| `paths` | string[] | The files in common, at most 100; `paths_truncated` |
| `count` | int | Number of files in common |

**Claim.** An advisory, expiring statement of intent on paths.

| Field | Type | Meaning |
|---|---|---|
| `id` | string | UUID |
| `workspace_slug` | string | The workspace whose paths are claimed. Claims made through a fork's slug are stored on the parent |
| `paths` | string[] | 1–50 globs |
| `target_branch` | string | Default: the default branch |
| `intent` | string | 1–2,000 characters |
| `by` | object | `{actor_type, actor_id, username}` |
| `session_id` | string or null | |
| `change` | int or null | Set when a change of the same session and target is opened while the claim is live |
| `expires_at` | timestamp | Default 1 h after creation, at most 24 h |
| `state` | enum | `active`, `released`, `expired`, `fulfilled` (its change landed or was abandoned) |
| `created_at` | timestamp | |

**Activity.** The answer to "what touches these paths": in-flight changes, live claims, and recently landed changes, each matched against a list of path globs.

### 9.4 Functional requirements

- **AWR-FR-001** All awareness reads need `changes:read` and read access to the workspace. Creating, renewing and releasing claims needs `changes:write` and read access to the workspace: claims are advisory and change nothing in the repository, so an org member may make them (§3, PLAT-FR-062). Through a fork's slug, awareness routes act on the fork's parent and need access to the fork only, so an agent holding only a fork token can use them. A fork-bound token may also call the awareness read routes on the parent's own slug: `GET …/activity`, `GET …/changes/:number/overlaps`, `GET …/claims` and `GET …/claims/:id`. Claim writes with such a token go through the fork's slug; with the parent's slug they answer `403 token_bound` (§3).
- **AWR-FR-002 (activity).** `GET …/activity` takes one or more `paths` globs and an optional `target` branch and returns:
  - `changes`: `draft` and `open` changes with a touched file matching any glob, as overlap entries;
  - `claims`: `active` claims with a glob that can match the same files as a requested glob. Two globs are treated as matching when either one matches the other's literal prefix; this errs on the side of reporting;
  - `landed`: changes landed on the target after `since` (a commit id, typically the caller's fork point or base, or a timestamp; default: 24 h ago) that touched a matching file, newest first, at most 100.
- **AWR-FR-003** With `exclude_session=<id>` or `exclude_change=<number>`, the caller's own work is left out of the activity answer.
- **AWR-FR-004 (overlaps of a change).** `GET …/changes/:number/overlaps` returns the overlap entries of every other `draft` or `open` change that overlaps this one, ordered by `count` descending.
- **AWR-FR-005 (overlap events).** When a new version makes two changes overlap that did not overlap before, the hub emits one `parley.change.overlap` for each of the two changes, naming the other. The pair is announced once; further versions that keep them overlapping emit nothing. If they stop overlapping and later overlap again, it is announced again.
- **AWR-FR-006** Overlap computation is asynchronous and never delays the push or commit that caused it.
- **AWR-FR-007 (claims).** Creating a claim returns the claim together with the current activity for its paths, so one call both announces and checks. Claims never conflict with each other: two sessions may claim the same paths.
- **AWR-FR-008** A claim can be renewed (`expires_at` moved forward, at most 24 h from now) and released by its creator, the workspace owner or the admin token. An expired or released claim is kept for 7 days for audit and then removed.
- **AWR-FR-009** When a change is opened with a `session_id` that has `active` claims on the same target branch, those claims get `change` set. When that change lands or is abandoned, the claims become `fulfilled`.
- **AWR-FR-010** Claims have no effect on pushes, commits, policy or landing.
- **AWR-FR-011** Limits: at most `[awareness].max_claims_per_workspace` (default 1,000) active claims per workspace and 50 per session; beyond that, `429`.

### 9.5 REST API

All routes are under `/workspaces/:slug`. The conventions of §3 apply.

#### `GET /workspaces/:slug/activity`
- **Purpose:** what touches these paths (AWR-FR-002).
- **Query:** `paths` (required; comma list or repeated), `target`, `since`, `exclude_session`, `exclude_change`.
- **Response:** `200 {"target_branch", "target_sha", "changes": [<overlap entry>…], "claims": [<claim>…], "landed": [{"number","ref","title","landed_sha","landed_at","session_id","paths"}…]}`.
- **Errors:** `400` when `paths` is missing or holds more than 50 globs.

#### `GET /workspaces/:slug/changes/:number/overlaps`
- **Purpose:** other in-flight changes touching the same files (AWR-FR-004).
- **Response:** `200` list of overlap entries.

#### `POST /workspaces/:slug/claims`
- **Purpose:** announce intended work (AWR-FR-007).
- **Body:** `{"paths": string[], "intent": string, "target_branch"?: string, "ttl"?: duration, "session_id"?: string}`.
- **Response:** `201 {"claim": <claim>, "activity": <activity answer for the claim's paths, excluding the claim itself>}`.
- **Errors:** `400` validation; `400 unknown_session` (`session_id` fails the session-linking rule of §14); `429` limits.
- **Side effects:** `parley.claim.create`.

#### `GET /workspaces/:slug/claims`
- **Purpose:** list claims.
- **Query:** `state` (default `active`), `session_id`, `path` (glob), `limit`, `cursor`. Newest first.
- **Response:** `200` list of claims.

#### `GET /workspaces/:slug/claims/:id`
- **Purpose:** read one claim. **Errors:** `404`, `error_type: claim_not_found`.

#### `PATCH /workspaces/:slug/claims/:id`
- **Purpose:** renew a claim or edit its intent.
- **Body:** `{"ttl"?: duration, "intent"?: string}`.
- **Response:** `200` claim. **Errors:** `409`, `error_type: claim_not_active`.

#### `DELETE /workspaces/:slug/claims/:id`
- **Purpose:** release a claim. Releasing a claim that is not active is a no-op.
- **Response:** `204`. **Side effects:** `parley.claim.release`.

### 9.6 CLI

#### `parley activity <slug>`
- **Flags:** `--paths <list>` (required), `--target <branch>`, `--since <sha|time>`, `--exclude-session <id>`, `--exclude-change <number>`, `--fail-on-overlap` (exit 3 when `changes` or `claims` is not empty).
- **Calls:** `GET …/activity`.

#### `parley change overlaps <slug> <number>`
- **Flags:** `--fail-on-overlap`. **Calls:** `GET …/changes/:number/overlaps`.

#### `parley claim create <slug>`
- **Flags:** `--paths <list>` (required), `--intent <text>` (required), `--target <branch>`, `--ttl <duration>`, `--session <id>`. **Calls:** `POST …/claims`.

#### `parley claim list <slug>`
- **Flags:** `--state`, `--session`, `--path`, `--limit`, `--cursor`. **Calls:** `GET …/claims`.

#### `parley claim renew <slug> <id>`
- **Flags:** `--ttl <duration>` (default `1h`). **Calls:** `PATCH …/claims/:id`.

#### `parley claim release <slug> <id>`
- **Calls:** `DELETE …/claims/:id`.

### 9.7 Background behaviour

- **Overlap index.** The hub keeps, per workspace and target branch, which `draft` and `open` changes touch which files. It is updated after each new version, retarget, land, abandon and restore.
- **Claim expiry** runs every minute: `active` claims past `expires_at` become `expired` and emit `parley.claim.expire`.
- **Claim cleanup** (AWR-FR-008) runs in the hourly maintenance loop (§3).

### 9.8 Events

| Event type | When | `resource_type` / `resource_id` | Metadata |
|---|---|---|---|
| `parley.change.overlap` | Two changes started to overlap (AWR-FR-005); one event per change | `change` / `<number>` | `with` (the other change's number and ref), `with_session_id`, `paths` (at most 100), `count` |
| `parley.claim.create` | A claim was created | `claim` / `<id>` | `paths`, `intent`, `target_branch`, `expires_at` |
| `parley.claim.release` | A claim was released | `claim` / `<id>` | `paths` |
| `parley.claim.expire` | A claim expired | `claim` / `<id>` | `paths` |

An agent that wants to hear about overlaps subscribes to `parley.change.overlap` filtered by its `session_id` (§14).

### 9.9 Configuration

| Key | Type | Default | Effect |
|---|---|---|---|
| `[awareness].default_claim_ttl` | duration | `1h` | TTL of a claim without `ttl` |
| `[awareness].max_claim_ttl` | duration | `24h` | Longest TTL |
| `[awareness].max_claims_per_workspace` | int | `1000` | Active claims per workspace |

### 9.10 Edge cases and error handling

- **File-level only.** Two changes that edit different functions of one file overlap. They may still both land cleanly. `mergeable` (§7) and the trial rebase of `GET …/scm/compare` (§6) answer whether they actually conflict.
- **Very large changes.** A change touching more than 2,000 files is indexed in full; only the lists returned to clients are capped.
- **Drafts** are included in activity and overlaps, so that early work is visible.
- **A claim on paths that do not exist yet** is valid. Agents use it for new directories.
- **Stale claims.** A crashed agent's claim disappears at `expires_at`. Long tasks renew.
- **Visibility.** Activity shows changes whose source is a fork the caller cannot read. This is intended: the change itself lives in the target workspace, which the caller can read.
