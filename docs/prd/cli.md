# afc CLI Reference

`afc` is the command-line client of the agent-fox hub. It creates and manages
workspaces, queues merges, rebases and carry-patch rebuilds, stores secrets and
variables, issues sandbox leases and reads and writes issues and pull requests
on a workspace's forge, all through the hub's REST API. This document lists
every command with its flags, output and exit codes; it is for people and
scripts that drive the hub from a shell.

`make build` installs `afc` into `$GOBIN` (or `$GOPATH/bin`). Sign in with
`afc login`, which stores the hub's URL and your API key in
`~/.af/config.toml`. For the requests behind each command see the
[REST API Reference](api.md); for what a personal access token may do see the
[Permissions Reference](permissions.md); for maintaining a fork with
carry-patch workspaces see the [Carry-Patch Workflow](carry_patch_workflow.md).

## Contents

- [Getting Started](#getting-started): [Install](#install),
  [Log in and configure](#log-in-and-configure)
- [Quick Tour](#quick-tour)
- [Global Behavior](#global-behavior): [Output](#output),
  [Exit codes](#exit-codes), [Waiting for a job](#waiting-for-a-job),
  [Path arguments](#path-arguments), [Permissions](#permissions),
  [Layout of a command entry](#layout-of-a-command-entry)
- [Workspace Commands](#workspace-commands)
- [Git Credential Helper](#git-credential-helper)
- [Secrets Commands](#secrets-commands): [Scope flags](#scope-flags),
  [Reserved secret names](#reserved-secret-names)
- [Variables Commands](#variables-commands)
- [Merge Commands](#merge-commands)
- [Rebase Commands](#rebase-commands)
- [Rebuild Commands](#rebuild-commands)
- [Rerere Commands](#rerere-commands)
- [Patch Commands](#patch-commands)
- [Sandbox Commands](#sandbox-commands)
- [Forge Commands](#forge-commands) (`forge`, `issue`, `pr`, `label`):
  [Errors of the forge commands](#errors-of-the-forge-commands),
  [Body input and flags of the write commands](#body-input-and-flags-of-the-write-commands)
- [Account and Admin Commands](#account-and-admin-commands)

**Workspaces**

| Command | Purpose |
|---------|---------|
| [`afc workspace create`](#afc-workspace-create) | Register a repository; the hub clones it |
| [`afc workspace list`](#afc-workspace-list) | List your workspaces |
| [`afc workspace get`](#afc-workspace-get) | Show one workspace |
| [`afc workspace update`](#afc-workspace-update) | Change display name, description, organization or settings |
| [`afc workspace archive`](#afc-workspace-archive) | Make a workspace read-only |
| [`afc workspace reactivate`](#afc-workspace-reactivate) | Bring an archived workspace back |
| [`afc workspace delete`](#afc-workspace-delete) | Delete an archived workspace for good |
| [`afc workspace sync`](#afc-workspace-sync) | Fetch origin (or upstream, for carry-patch) into the hub's clone |
| [`afc workspace reclone`](#afc-workspace-reclone) | Throw away the hub's clone and clone again |
| [`afc workspace push`](#afc-workspace-push) | Push one branch from the hub to origin |
| [`afc workspace patch-status`](#afc-workspace-patch-status) | Carry-patch status dashboard |

**Git credential helper**

| Command | Purpose |
|---------|---------|
| [`afc credential-helper`](#afc-credential-helper) | Let git authenticate to the hub with your `afc` login |

**Secrets and variables**

| Command | Purpose |
|---------|---------|
| [`afc secrets create`](#afc-secrets-create) | Create one or more secrets |
| [`afc secrets list`](#afc-secrets-list) | List secret keys (never values) |
| [`afc secrets update`](#afc-secrets-update) | Change a secret's value |
| [`afc secrets delete`](#afc-secrets-delete) | Delete a secret |
| [`afc vars create`](#afc-vars-create) | Create one or more variables |
| [`afc vars list`](#afc-vars-list) | List variables with their values |
| [`afc vars update`](#afc-vars-update) | Change a variable's value |
| [`afc vars delete`](#afc-vars-delete) | Delete a variable |
| [`afc vars resolve`](#afc-vars-resolve) | Show a workspace's variables after workspace > org > user resolution |

**Merge and rebase**

| Command | Purpose |
|---------|---------|
| [`afc merge submit`](#afc-merge-submit) | Queue a merge of a branch into a target branch |
| [`afc merge list`](#afc-merge-list) | List a workspace's merge jobs |
| [`afc merge status`](#afc-merge-status) | Show one merge job |
| [`afc merge cancel`](#afc-merge-cancel) | Cancel a queued merge job |
| [`afc merge requeue`](#afc-merge-requeue) | Queue a dead-lettered merge job again |
| [`afc rebase submit`](#afc-rebase-submit) | Rebase several branches onto a ref |

**Carry-patch: rebuild, rerere, patch**

| Command | Purpose |
|---------|---------|
| [`afc rebuild submit`](#afc-rebuild-submit) | Queue a rebuild of the integration branch |
| [`afc rebuild list`](#afc-rebuild-list) | List a workspace's rebuild jobs |
| [`afc rebuild status`](#afc-rebuild-status) | Show one rebuild job with its per-patch results |
| [`afc rebuild preview`](#afc-rebuild-preview) | Predict conflicts without rebuilding |
| [`afc rebuild cancel`](#afc-rebuild-cancel) | Cancel a queued rebuild job |
| [`afc rebuild requeue`](#afc-rebuild-requeue) | Queue a dead-lettered rebuild job again |
| [`afc rebuild rollback`](#afc-rebuild-rollback) | Put the integration branch back to before a rebuild |
| [`afc rerere list`](#afc-rerere-list) | List recorded conflict resolutions |
| [`afc rerere forget`](#afc-rerere-forget) | Forget a recorded resolution |
| [`afc patch add`](#afc-patch-add) | Register a patch branch |
| [`afc patch list`](#afc-patch-list) | List patches in order |
| [`afc patch remove`](#afc-patch-remove) | Remove a patch |
| [`afc patch update`](#afc-patch-update) | Change a patch's status, description, PR URL or position |
| [`afc patch reorder`](#afc-patch-reorder) | Set the order of all patches |
| [`afc patch restore`](#afc-patch-restore) | Bring a removed patch back |
| [`afc patch reset-to-origin`](#afc-patch-reset-to-origin) | Move a patch branch to the fork's tip |

**Sandboxes**

| Command | Purpose |
|---------|---------|
| [`afc sandbox profiles`](#afc-sandbox-profiles) | List the sandbox profiles the hub offers |
| [`afc sandbox create`](#afc-sandbox-create) | Issue a sandbox lease |
| [`afc sandbox list`](#afc-sandbox-list) | List a workspace's leases |
| [`afc sandbox get`](#afc-sandbox-get) | Show one lease |
| [`afc sandbox destroy`](#afc-sandbox-destroy) | End a lease and destroy its sandbox |

**Forge: forge, issue, pr, label**

| Command | Purpose |
|---------|---------|
| [`afc forge status`](#afc-forge-status) | Show how the hub reaches a workspace's forge |
| [`afc forge webhook set`, `unset`](#afc-forge-webhook-set-unset) | Set up or turn off the forge's webhook |
| [`afc forge deliveries`](#afc-forge-deliveries) | List the webhook deliveries the hub received |
| [`afc issue list`](#afc-issue-list) | List issues |
| [`afc issue get`](#afc-issue-get) | Show an issue with its comments |
| [`afc issue create`](#afc-issue-create) | Create an issue |
| [`afc issue update`](#afc-issue-update) | Change an issue |
| [`afc issue close`](#afc-issue-close) | Close an issue, with a comment if given |
| [`afc issue comment`](#afc-issue-comment) | Comment on an issue |
| [`afc issue label`, `unlabel`](#afc-issue-label-unlabel) | Add labels to an issue, or remove one |
| [`afc label create`](#afc-label-create) | Make sure a label exists |
| [`afc pr get`, `state`, `files`, `checks`, `reviews`](#afc-pr-get-state-files-checks-reviews) | Read a pull request |
| [`afc pr create`](#afc-pr-create) | Open a pull request |
| [`afc pr comment`](#afc-pr-comment) | Comment on a pull request |

**Account and administration**

| Command | Purpose |
|---------|---------|
| [`afc login`](#afc-login) | Sign in and store an API key |
| [`afc user`](#afc-user) | Show or change your profile |
| [`afc keys`](#afc-keys) | List, rotate or revoke your API key |
| [`afc tokens`](#afc-tokens) | Manage personal access tokens, also workspace-bound ones |
| [`afc orgs`](#afc-orgs) | List and show your organizations |
| [`afc admin`](#afc-admin) | Manage users, organizations, keys and tokens (admins only) |

---

## Getting Started

### Install

```
make build
```

builds the hub and installs `afc` into `$GOBIN` (or `$GOPATH/bin`). Run
`afc --help`, or `afc <command> --help`, for the built-in help.

### Log in and configure

```
afc login --endpoint-url https://hub.example.com
```

opens a browser for OAuth sign-in (GitHub by default), receives an API key from
the hub and saves the hub's URL, your user ID and the key to
`~/.af/config.toml`; see [`afc login`](#afc-login). Later commands, and later
logins, read the hub's URL from there. Logging in again revokes your older API
keys.

Every command finds the hub and the credential in this order: the flag, then the
`AF_`-prefixed environment variable, then the bare one, then the config file.

| Setting | Flag | Environment variables | Config key |
|---------|------|-----------------------|------------|
| Hub URL | `--endpoint-url` | `AF_ENDPOINT_URL`, `ENDPOINT_URL` | `endpoint_url` |
| Credential (API key or PAT) | `--api-key` | `AF_API_KEY`, `API_KEY` | `api_key` |
| User ID (optional) | `--user-id` | `AF_USER_ID`, `USER_ID` | `user_id` |

A command that needs the hub fails when the URL or the credential is missing
from all of them, so CI jobs and containers can run `afc` with environment
variables alone. The config file lives in `~/.af/`, or in the directory
`AF_CONFIG_DIR` names; `afc` creates the directory (mode `0700`) and the file
(mode `0600`). See [CLI Configuration](configuration.md#cli-configuration-afconfigtoml).
The [git credential helper](#afc-credential-helper) reads only `ENDPOINT_URL`,
`API_KEY` and `~/.af/config.toml`.

---

## Quick Tour

A workspace from creation to a merged branch on GitHub:

```sh
# Sign in once; afc remembers the hub's URL
afc login --endpoint-url http://localhost:8080

# Register a repository; --wait returns once the hub has cloned it.
# The printed workspace includes hub_url, its address on the hub's git server.
afc workspace create --slug demo --git-url https://github.com/acme/demo.git \
    --git-pat "$GITHUB_TOKEN" --wait

# Let git use your afc login for the hub, then clone from the hub
git config --global credential.http://localhost:8080.helper '!afc credential-helper'
git clone http://localhost:8080/git/acme/demo.git && cd demo

# Work on a branch and push it to the hub
git switch -c feature/greeting
# ... edit, commit ...
git push -u origin feature/greeting

# Merge it into main through the hub's queue, then push main to GitHub
afc merge submit demo --source feature/greeting --target main --wait
afc workspace push demo main
```

Replace `http://localhost:8080` with your hub's URL. For a fork that carries
patches on top of an upstream project, see the
[Carry-Patch Workflow](carry_patch_workflow.md).

---

## Global Behavior

### Output

- On success a command prints the hub's answer as JSON on stdout.
- Where the hub answers `204 No Content`, the command prints a small status
  document instead, for example `{"status": "deleted", "slug": "x"}`, and most
  also print a confirmation line on stderr. Each entry shows its document.
  `afc secrets delete` and `afc vars delete` print only the stderr line.
- On error a command prints a JSON error envelope (`{"error": {"code": ...,
  "message": ...}}`) on stdout. Hints and warnings go to stderr, so stdout stays
  parseable.

### Exit codes

Every command uses the same exit codes. Entries mention exit codes only to name
their particular usage errors or refusals.

| Code | Meaning |
|------|---------|
| 0 | Success. With `--wait`: the job `completed` (or the clone is `ready`) |
| 1 | The hub answered with an error (HTTP 4xx or 5xx: not found, refused, conflict, missing scope, server error); the envelope on stdout carries the hub's message. Also: a `--wait` job ended `failed`, `dead_letter` or `cancelled`, a clone `failed`, or the wait timed out |
| 2 | Usage or client error: a wrong number of arguments or a missing, conflicting or invalid flag (nothing is sent), no API key configured (run `afc login`), the hub could not be reached or did not answer in time, or an answer `afc` could not read |
| 3 | Only `afc workspace sync --fail-on-diverged`: the sync succeeded but patch branches have diverged |

### Waiting for a job

`afc workspace create`, `afc workspace sync`, `afc merge submit` and
`afc rebuild submit` take `--wait`:

| Flag | Default | Description |
|------|---------|-------------|
| `--wait` | `false` | Block until the job (or the clone) reaches a final state |
| `--timeout` | `5m0s` | How long to wait; only with `--wait` |
| `--poll-interval` | `5s` | Time between status checks; only with `--wait` |

With `--wait` the command prints the hub's first answer on one line, polls the
job's status at once and then every `--poll-interval`, and prints the final
record. It exits 0 only when the job `completed` (or the clone is `ready`). A
job that ends `failed`, `dead_letter` or `cancelled`, or a clone that `failed`,
prints the record, a line such as `Job ended with status failed.` on stderr, and
exits 1. When `--timeout` passes first, stderr says `Timed out waiting for ...
after <timeout>` and the command exits 1.

### Path arguments

Every slug, key, id, branch or label a command places in a request URL is
percent-encoded as one path segment, so a `/` in a branch name is sent as
`%2F`. The one exception is [`afc rerere forget`](#afc-rerere-forget), which
keeps the slashes of a file path.

### Permissions

An API key (what `afc login` stores) may run every command on the workspaces,
secrets and variables you own; an admin token may run all of them. A personal
access token (PAT) needs the scope each entry names. Members of a workspace's
organization have read-only access to it. See the
[Permissions Reference](permissions.md) for the rules.

### Layout of a command entry

Each command entry has the same parts, in this order: what it does, usage,
**API** (the REST call it makes, linked to the [API reference](api.md), and the
PAT scope it needs), a flags table where it has flags, **Behavior**, and
**Errors** where the command has usage errors or refusals worth naming.

---

## Workspace Commands

A workspace is a repository the hub holds a clone of. Agents clone it from the
hub's git server, push branches there and merge them through the hub; the hub
syncs with and pushes to the repository's `origin`. A `carry_patch` workspace
also tracks an `upstream` and rebuilds an integration branch from upstream plus
a list of patches (see [Patch Commands](#patch-commands)).

### afc workspace create

Register a repository as a new workspace; the hub clones it.

```
afc workspace create --slug <slug> --git-url <url> [flags]
```

**API:** [`POST /api/v1/workspaces`](api.md#post-apiv1workspaces) · PAT scope
`workspaces:write`

| Flag | Description |
|------|-------------|
| `--slug` | **Required.** Globally unique, URL-safe identifier of the workspace |
| `--git-url` | **Required.** HTTPS or SSH URL of the repository (the workspace's `origin`) |
| `--branch` | Git ref to associate with the workspace |
| `--org` | Organization slug to put the workspace in (sent as `org_id`). Default: your personal organization |
| `--display-name` | Human-readable label. Default: the slug |
| `--description` | Free-form description. Default: empty |
| `--workspace-mode` | `standard` (default) or `carry_patch` |
| `--upstream-url` | Upstream repository URL. Required for `carry_patch`, not allowed for `standard` |
| `--integration-branch` | Integration branch of a `carry_patch` workspace. Default: `deploy`. Not allowed for `standard` |
| `--git-pat` | Personal access token for a private repository |
| `--git-username` | Username for HTTP basic auth; needs `--git-password` |
| `--git-password` | Password for HTTP basic auth; needs `--git-username` |
| `--setting` | A workspace setting as `key=value`, repeatable, for example `--setting patch_branch_source=origin --setting auto_rebuild_after_push=false`. `true` and `false` are sent as booleans. See the [Workspace Settings Reference](api.md#workspace-settings-reference) |
| `--wait`, `--timeout`, `--poll-interval` | Wait until the clone is `ready` or `failed`; see [Waiting for a job](#waiting-for-a-job) |

**Behavior:**

- Prints the new workspace. Its `status` starts as `pending` and follows the
  clone (`cloning`, then `ready` or `failed`); `hub_url` is its address on the
  hub's git server.
- The credential flags are stored as the workspace secrets `GIT_PAT`, or
  `GIT_USERNAME` and `GIT_PASSWORD` (see
  [Reserved secret names](#reserved-secret-names)). The hub checks them against
  the repository before it creates the workspace.

**Errors:** usage errors (exit 2, nothing sent): a missing `--slug` or
`--git-url`; `--git-pat` together with `--git-username`/`--git-password`;
only one of `--git-username` and `--git-password`; an empty credential value;
credential flags with a `--git-url` that is not `https://`; a `--workspace-mode`
other than `standard` or `carry_patch`; `carry_patch` without `--upstream-url`;
`--upstream-url` or `--integration-branch` on a `standard` workspace; a
`--setting` without `=`.

### afc workspace list

List the workspaces you own.

```
afc workspace list [--include-archived]
```

**API:** [`GET /api/v1/workspaces`](api.md#get-apiv1workspaces) · PAT scope
`workspaces:read`

| Flag | Description |
|------|-------------|
| `--include-archived` | Include archived workspaces |

**Behavior:** prints a JSON array of workspaces.

### afc workspace get

Show one workspace.

```
afc workspace get <slug>
```

**API:** [`GET /api/v1/workspaces/:slug`](api.md#get-apiv1workspacesslug) · PAT
scope `workspaces:read`

**Behavior:** prints the workspace.

### afc workspace update

Change a workspace's display name, description, organization or settings. Only
the fields you give change.

```
afc workspace update <slug> [flags]
```

**API:** [`PATCH /api/v1/workspaces/:slug`](api.md#patch-apiv1workspacesslug) ·
PAT scope `workspaces:write`

| Flag | Description |
|------|-------------|
| `--display-name` | New display name, at most 128 characters |
| `--description` | New description, at most 1024 characters |
| `--org` | Organization slug to move the workspace to (sent as `org_id`) |
| `--clear-display-name` | Reset the display name to its default, the slug |
| `--clear-description` | Reset the description to its default, empty |
| `--clear-org` | Remove the organization association |
| `--setting` | A workspace setting as `key=value`, repeatable; `key=` alone resets the setting to its default. See the [Workspace Settings Reference](api.md#workspace-settings-reference) |

**Behavior:**

- Sends only the fields named by the flags. A `--clear-*` flag sends the field
  as `null`, which resets it; it wins over the matching value flag.
- `--setting` values are sent in the `settings` object: `true` and `false` as
  booleans, an empty value as `null`. The hub rejects an unknown key or value
  with 400.
- Prints the updated workspace. The request gives up after 10 seconds.

**Errors:** no update flag at all is a usage error (exit 2, nothing sent), as is
a `--setting` without `=`. An answer that is not a workspace object exits 2.

### afc workspace archive

Archive a workspace, making it read-only.

```
afc workspace archive <slug>
```

**API:** [`POST /api/v1/workspaces/:slug/archive`](api.md#post-apiv1workspacesslugarchive)
· PAT scope `workspaces:write`

**Behavior:** prints the workspace with `status` `archived`.

**Errors:** the hub refuses (exit 1) a workspace that is archived already or
still cloning.

### afc workspace reactivate

Bring an archived workspace back. The hub clones it again.

```
afc workspace reactivate <slug>
```

**API:** [`POST /api/v1/workspaces/:slug/reactivate`](api.md#post-apiv1workspacesslugreactivate)
· PAT scope `workspaces:write`

**Behavior:** prints the workspace with `status` `pending`; the status then
follows the new clone.

**Errors:** the hub refuses (exit 1) a workspace that is not archived.

### afc workspace delete

Delete an archived workspace for good.

```
afc workspace delete <slug> --confirm
```

**API:** [`DELETE /api/v1/workspaces/:slug`](api.md#delete-apiv1workspacesslug) ·
PAT scope `workspaces:write`

| Flag | Description |
|------|-------------|
| `--confirm` | **Required.** Confirms the deletion |

**Behavior:** prints `{"status": "deleted", "slug": "<slug>"}` on stdout and a
confirmation on stderr.

**Errors:** without `--confirm` the command exits 2 and sends nothing. The hub
refuses (exit 1) a workspace that is not archived.

### afc workspace sync

Fetch the workspace's remote into the hub's clone. Which remote depends on the
workspace mode:

| Workspace mode | Syncs from | What happens |
|---|---|---|
| `standard` | `origin`, the `--git-url` given at creation | Fetches `origin` and fast-forwards the workspace branch (`--branch`, or the repository's default branch) to it. A diverged branch (a force-push on `origin`) fails with 409; recover with `--reset-to-upstream`. |
| `carry_patch` | `upstream`, the `--upstream-url` given at creation (**not** `origin`) | Fetches all `upstream` branches, records the upstream default branch as the new base, marks patches that were merged upstream as `merged_upstream`, and, if upstream advanced or a patch branch changed, queues a rebuild of the integration branch (upstream HEAD plus the active patches). The integration branch itself changes only when that rebuild completes. With `patch_branch_source=origin` the fork is fetched as well and every registered patch branch is brought to the fork's tip. |

```
afc workspace sync <slug> [--reset-to-upstream] [--fail-on-diverged] [--wait] [--timeout <duration>] [--poll-interval <duration>]
```

**API:** [`POST /api/v1/workspaces/:slug/sync`](api.md#post-apiv1workspacesslugsync)
(with `?reset_to_upstream=true` for `--reset-to-upstream`) · PAT scope
`workspaces:write`

| Flag | Description |
|------|-------------|
| `--reset-to-upstream` | Standard workspaces only: force-reset the workspace branch to `origin`'s HEAD whatever their ancestry (recovery after a force-push). Ignored for carry-patch workspaces, where an upstream force-push is reported as `force_push_detected` and handled by the rebuild |
| `--fail-on-diverged` | Exit 3 when the response lists `patches_diverged` (carry-patch workspaces with `patch_branch_source=origin` and `patch_divergence_policy=report`) |
| `--wait`, `--timeout`, `--poll-interval` | Wait for the rebuild the sync queued, if any; see [Waiting for a job](#waiting-for-a-job) |

**Behavior:**

- Prints the workspace with the sync fields `upstream_head_sha`,
  `last_sync_at` and `sync_error`. A carry-patch workspace adds
  `patches_merged`, `rebuild_triggered`, `force_push_detected`,
  `origin_fetched` and, when a rebuild was queued, `rebuild_job_id`; with
  `patch_branch_source=origin` also `patches_synced` and `patches_diverged`.
- With `--wait`: when the response has a `rebuild_job_id`, the command waits
  for that rebuild and prints its final record. Without one it ends after
  printing the sync response.
- With `--fail-on-diverged`: after the response (and after the rebuild wait,
  with `--wait`), a non-empty `patches_diverged` makes the command print
  `diverged patch branches: a, b` on stderr and exit 3. stdout then holds only
  the sync response, with no error envelope after it, so a script can parse it
  and read the branch names from `patches_diverged`. A failed wait (exit 1)
  takes precedence over exit 3.

Notes for carry-patch workspaces:

- By default the fork (`origin`) is not fetched. Patch branches are read from
  the hub's clone, so push them to the hub (`git push <hub-remote> <branch>`,
  using the workspace's `hub_url`); pushing a registered patch branch also
  triggers a rebuild. With `patch_branch_source=origin` the fork is fetched
  during sync instead; see the `patch_branch_source` and
  `patch_divergence_policy` settings.
- Upstream credentials come from the workspace secrets `UPSTREAM_GIT_PAT` (or
  `UPSTREAM_GIT_USERNAME` and `UPSTREAM_GIT_PASSWORD`), set with
  `afc secrets create --workspace <slug> ...` (see
  [Reserved secret names](#reserved-secret-names)); without them the `origin`
  credentials are used.
- The rebuilt integration branch is pushed to `origin` only when the setting
  `rebuild_push_integration_branch` is `true`.
- With the setting `auto_rebuild_after_sync=false` a sync queues no rebuild.

**Errors:** the hub refuses (exit 1) a workspace that has syncing disabled,
whose clone is not ready, or that is syncing already. Exit 3 as described
above.

### afc workspace reclone

Delete the hub's clone of a workspace and clone it again: the last resort for a
clone that is beyond repair.

```
afc workspace reclone <slug> --confirm
```

**API:** [`POST /api/v1/workspaces/:slug/reclone`](api.md#post-apiv1workspacesslugreclone)
· PAT scope `workspaces:write`

| Flag | Description |
|------|-------------|
| `--confirm` | **Required.** Confirms the reclone; checked by `afc` before anything is sent |

**Behavior:**

- The hub first tries to push local commits to the repository (a failed push is
  only logged), then deletes its clone and clones again.
- Prints the workspace with `status` `pending`; the status then follows the new
  clone (`cloning`, then `ready` or `failed`).

**Errors:** without `--confirm` the command exits 2 and sends nothing. The hub
refuses (exit 1) a workspace whose clone is in progress.

### afc workspace push

Push one branch of the hub's clone to the workspace's `origin`, with the
workspace's stored git credential, for example to open a pull request from it.

```
afc workspace push <slug> <branch> [--force]
```

**API:** [`POST /api/v1/workspaces/:slug/branches/:name/push`](api.md#post-apiv1workspacesslugbranchesnamepush)
with `{"force": <bool>}` · PAT scope `workspaces:write`

| Flag | Description |
|------|-------------|
| `--force` | Overwrite `origin`'s branch even when it has commits the workspace lacks. The hub refuses it for the workspace's own branch |

**Behavior:**

- `<branch>` may contain `/` (`fix/42-nil-map`); it is sent as one path segment
  (`fix%2F42-nil-map`).
- With `--force` the command first writes
  `warning: --force: origin's <branch> may be overwritten with the workspace's branch`
  on stderr.
- Prints `branch`, `sha`, `remote` (`origin`), `remote_url` (without
  credentials) and `result`: `pushed`, `up_to_date` or `forced`. Pushing an
  unchanged branch again prints `up_to_date`.
- A refusal's message begins with its error type, for example
  `non_fast_forward: origin's feature is at <sha>, which is not in the
  workspace's branch; push with force to overwrite it`.

**Errors:** the hub answers with an error (exit 1) for an unknown workspace or
branch, a push that is not a fast-forward, a push `origin` refuses or that
fails, and a busy workspace.

### afc workspace patch-status

Show the carry-patch dashboard of a workspace.

```
afc workspace patch-status <workspace-slug>
```

**API:** [`GET /api/v1/workspaces/:slug/patch-status`](api.md#get-apiv1workspacesslugpatch-status)
· PAT scope `workspaces:read`

**Behavior:** prints the workspace's carry-patch state (`workspace_slug`,
`workspace_mode`, `upstream_url`, `upstream_head_sha`, `integration_branch`,
`integration_head_sha`, `last_sync_at`), a `last_rebuild` summary, each patch's
status with its `last_rebuild_result`, and `summary` counts (including
`total_rerere_resolutions`).

**Errors:** the hub refuses (exit 1, HTTP 400) a workspace that is not in
`carry_patch` mode.

---

## Git Credential Helper

### afc credential-helper

Let git authenticate to the hub's git server with your `afc` login, so plain
hub URLs work without a token in them.

```
git config --global credential.http://localhost:8080.helper '!afc credential-helper'
```

Replace `http://localhost:8080` with your hub's URL. Then:

```
git clone http://localhost:8080/git/acme/my-workspace.git
git push origin main
```

**Behavior:**

- Git calls `afc credential-helper get` whenever it needs credentials for the
  hub; you do not run it yourself, and `afc --help` does not list it. It
  follows git's
  [credential helper protocol](https://git-scm.com/docs/gitcredentials).
- It reads the hub URL and API key from `ENDPOINT_URL` and `API_KEY` when they
  are set, otherwise from `~/.af/config.toml`, and answers with the key as the
  HTTP basic password (user `x-token-auth`).
- It answers only requests whose scheme and host:port match the hub URL. A
  plain-`http` request for an `https` hub is ignored, so the key is never sent
  over a downgraded connection; requests for other hosts are left to git's
  other helpers.
- `store` and `erase` do nothing; `afc login` manages the key.

---

## Secrets Commands

Secrets are key/value pairs owned by your user, an organization or a workspace.
Values are write-only: the hub never returns them, and commands show keys
and timestamps only. A few workspace secret names have a meaning to the hub; see
[Reserved secret names](#reserved-secret-names).

### Scope flags

The secrets and variables commands (except `afc vars resolve`) pick the owner
with these flags:

| Flag | Description |
|------|-------------|
| `--user` | Your user (the default when no scope flag is given) |
| `--org <slug>` | An organization |
| `--workspace <slug>` | A workspace |

`list`, `update` and `delete` take at most one scope flag; more than one is a
usage error (exit 2). `create` takes several and writes to each in turn, in the
order user, organization, workspace. The API path is
`/api/v1/user/...`, `/api/v1/orgs/<slug>/...` or
`/api/v1/workspaces/<slug>/...`.

### Reserved secret names

The hub reads a fixed set of workspace secret names to authenticate its own git
and forge operations. Manage them with the ordinary `afc secrets` commands and
`--workspace <slug>`; every other name is an ordinary secret the hub never
interprets.

| Name | Used for |
|------|----------|
| `GIT_PAT` | Personal access token for the workspace's `origin` (clone, fetch, push). Sent as HTTP basic auth with username `x-token-auth` |
| `GIT_USERNAME` + `GIT_PASSWORD` | HTTP basic auth for `origin`, instead of `GIT_PAT`. Both must be set; a username without a password is ignored (the hub logs a warning) and counts as no credentials |
| `UPSTREAM_GIT_PAT` | Personal access token for the `upstream` of a `carry_patch` workspace |
| `UPSTREAM_GIT_USERNAME` + `UPSTREAM_GIT_PASSWORD` | HTTP basic auth for `upstream`, instead of `UPSTREAM_GIT_PAT`. Both must be set |
| `FORGE_WEBHOOK_SECRET` | The secret the workspace's forge signs its webhook deliveries with (GitHub's HMAC-SHA256 key, GitLab's token). Set it with [`afc forge webhook set`](#afc-forge-webhook-set-unset); `afc secrets` works too. One secret covers deliveries from the origin and, on a `carry_patch` workspace, from the upstream |

Which credential is used:

- **`origin`:** `GIT_PAT`, else `GIT_USERNAME` + `GIT_PASSWORD`, else none
  (a public repository).
- **`upstream`:** `UPSTREAM_GIT_PAT`, else `UPSTREAM_GIT_USERNAME` +
  `UPSTREAM_GIT_PASSWORD`, else the `origin` credential.

`afc workspace create --git-pat`, `--git-username` and `--git-password` store
`GIT_PAT`, `GIT_USERNAME` and `GIT_PASSWORD`. Reserved names count only in
workspace scope: an organization's or a user's `FORGE_WEBHOOK_SECRET` is never
read.

```
# Set an upstream PAT without leaving it in shell history
printf '%s' "$GITHUB_TOKEN" | afc secrets create UPSTREAM_GIT_PAT --from-stdin --workspace my-ws

# Basic auth for the upstream (set both halves)
afc secrets create UPSTREAM_GIT_USERNAME=my-bot --workspace my-ws
printf '%s' "$TOKEN" | afc secrets create UPSTREAM_GIT_PASSWORD --from-stdin --workspace my-ws

# Rotate, inspect, or remove (falls back to the origin credentials)
printf '%s' "$NEW_TOKEN" | afc secrets update UPSTREAM_GIT_PAT --from-stdin --workspace my-ws
afc secrets list --workspace my-ws
afc secrets delete UPSTREAM_GIT_PAT --workspace my-ws
```

To remove basic-auth credentials, delete both `UPSTREAM_GIT_USERNAME` and
`UPSTREAM_GIT_PASSWORD`; a half left behind is ignored and the hub falls back to
the `origin` credentials.

### afc secrets create

Create one or more secrets.

```
afc secrets create <KEY=VALUE[,KEY2=VALUE2,...]> [KEY3=VALUE3 ...] [scope flags]
afc secrets create <KEY> --from-stdin [scope flags]
```

**API:** `POST .../secrets` with an `entries` array, per scope
([Secrets Endpoints](api.md#secrets-endpoints)) · PAT scope `secrets:write`

| Flag | Description |
|------|-------------|
| `--user`, `--org`, `--workspace` | Owners to write to; several allowed. See [Scope flags](#scope-flags) |
| `--from-stdin` | Read the value of the single `KEY` argument from stdin (one trailing newline removed). Keeps the value out of shell history |

**Behavior:**

- Each argument is a comma-separated list of `KEY=VALUE` pairs. A key may contain
  letters, digits and underscores; a value may contain further `=` characters.
- With several scope flags, the command writes to each scope in turn and prints
  the created entries for each. A scope that fails prints its error and the
  others are still tried; the command then exits 1.

**Errors:** usage errors (exit 2): no argument, a pair without `=`, an empty or
whitespace-only key, `--from-stdin` with anything but a single bare `KEY`.

### afc secrets list

List the secrets of one owner: keys and timestamps, never values.

```
afc secrets list [--user | --org <slug> | --workspace <slug>]
```

**API:** `GET .../secrets` ([Secrets Endpoints](api.md#secrets-endpoints)) · PAT
scope `secrets:read` or `secrets:write`

**Behavior:** prints a JSON array of entries. Defaults to your user.

### afc secrets update

Change the value of a secret.

```
afc secrets update <KEY=VALUE> [scope flag]
afc secrets update <KEY> --from-stdin [scope flag]
```

**API:** `PATCH .../secrets/:key` ([Secrets Endpoints](api.md#secrets-endpoints))
· PAT scope `secrets:write`

| Flag | Description |
|------|-------------|
| `--user`, `--org`, `--workspace` | The owner; at most one. See [Scope flags](#scope-flags) |
| `--from-stdin` | Read the new value from stdin (one trailing newline removed) |

**Behavior:** prints the updated entry.

**Errors:** usage errors (exit 2): no argument, a `KEY=VALUE` without `=`,
`--from-stdin` with a `KEY=VALUE` argument or an empty key, more than one scope
flag.

### afc secrets delete

Delete a secret.

```
afc secrets delete <KEY> [scope flag]
```

**API:** `DELETE .../secrets/:key` ([Secrets Endpoints](api.md#secrets-endpoints))
· PAT scope `secrets:write`

**Behavior:** prints `Secret '<KEY>' has been deleted.` on stderr and nothing on
stdout.

**Errors:** usage errors (exit 2): no argument, more than one scope flag.

---

## Variables Commands

Variables are key/value pairs like secrets, but their values are readable. A
workspace sees its own variables, its organization's and its owner's, resolved
by [`afc vars resolve`](#afc-vars-resolve). The commands take the same
[scope flags](#scope-flags) as the secrets commands, with the same rules.

### afc vars create

Create one or more variables.

```
afc vars create <KEY=VALUE[,KEY2=VALUE2,...]> [KEY3=VALUE3 ...] [scope flags]
afc vars create <KEY> --from-stdin [scope flags]
```

**API:** `POST .../vars` with an `entries` array, per scope
([Variables Endpoints](api.md#variables-endpoints)) · PAT scope `vars:write`

| Flag | Description |
|------|-------------|
| `--user`, `--org`, `--workspace` | Owners to write to; several allowed. See [Scope flags](#scope-flags) |
| `--from-stdin` | Read the value of the single `KEY` argument from stdin (one trailing newline removed) |

**Behavior:** as for [`afc secrets create`](#afc-secrets-create): comma-separated
`KEY=VALUE` arguments, keys of letters, digits and underscores, each scope
written in turn, exit 1 when any scope failed.

**Errors:** as for `afc secrets create`.

### afc vars list

List the variables of one owner with their values.

```
afc vars list [--user | --org <slug> | --workspace <slug>]
```

**API:** `GET .../vars` ([Variables Endpoints](api.md#variables-endpoints)) · PAT
scope `vars:read` or `vars:write`

**Behavior:** prints a JSON array of entries (keys, values and timestamps).
Defaults to your user.

### afc vars update

Change the value of a variable.

```
afc vars update <KEY=VALUE> [scope flag]
afc vars update <KEY> --from-stdin [scope flag]
```

**API:** `PATCH .../vars/:key` ([Variables Endpoints](api.md#variables-endpoints))
· PAT scope `vars:write`

| Flag | Description |
|------|-------------|
| `--user`, `--org`, `--workspace` | The owner; at most one. See [Scope flags](#scope-flags) |
| `--from-stdin` | Read the new value from stdin (one trailing newline removed) |

**Behavior:** prints the updated entry.

**Errors:** as for [`afc secrets update`](#afc-secrets-update).

### afc vars delete

Delete a variable.

```
afc vars delete <KEY> [scope flag]
```

**API:** `DELETE .../vars/:key` ([Variables Endpoints](api.md#variables-endpoints))
· PAT scope `vars:write`

**Behavior:** prints `Variable '<KEY>' has been deleted.` on stderr and nothing
on stdout.

**Errors:** usage errors (exit 2): no argument, more than one scope flag.

### afc vars resolve

Show the variables a workspace sees after resolution: a workspace variable wins
over an organization variable of the same key, which wins over a user variable.

```
afc vars resolve <workspace-slug>
```

**API:** [`GET /api/v1/workspaces/:slug/vars/resolved`](api.md#get-apiv1workspacesslugvarsresolved)
· PAT scope `vars:read` or `vars:write`

**Behavior:** prints the resolved variables, each with the tier it came from.
Takes no scope flags; arguments after the slug are ignored.

**Errors:** no workspace slug is a usage error (exit 2).

---

## Merge Commands

A merge job brings a branch into a target branch of the hub's clone by rebasing
it onto the target and fast-forwarding the target. The hub runs the jobs in the
background; `afc merge submit --wait` follows one to its end.

### afc merge submit

Queue a merge job.

```
afc merge submit <workspace-slug> --target <branch> --source <branch> [--wait] [--timeout <duration>] [--poll-interval <duration>]
```

**API:** [`POST /api/v1/workspaces/:slug/merges`](api.md#post-apiv1workspacesslugmerges)
with `target_branch` and `source_ref` · PAT scope `merges:write`

| Flag | Description |
|------|-------------|
| `--target` | **Required.** Branch to merge into |
| `--source` | **Required.** Branch to merge from |
| `--wait`, `--timeout`, `--poll-interval` | Wait for the job to finish; see [Waiting for a job](#waiting-for-a-job) |

**Behavior:** prints the job, with its `id` and `status` `queued`. With `--wait`
the command follows it to `completed`, `failed`, `dead_letter` or `cancelled`.

**Errors:** a missing `--target` or `--source` is a usage error (exit 2).

### afc merge list

List a workspace's merge jobs.

```
afc merge list <workspace-slug>
```

**API:** [`GET /api/v1/workspaces/:slug/merges`](api.md#get-apiv1workspacesslugmerges)
· PAT scope `merges:read`

**Behavior:** prints a JSON array of jobs.

### afc merge status

Show one merge job.

```
afc merge status <workspace-slug> <merge-id>
```

**API:** [`GET /api/v1/workspaces/:slug/merges/:id`](api.md#get-apiv1workspacesslugmergesid)
· PAT scope `merges:read`

**Behavior:** prints the job with its status, `base_sha` and `merged_sha` once it
completed, and `conflict_files` when it failed.

### afc merge cancel

Cancel a queued merge job.

```
afc merge cancel <workspace-slug> <merge-id>
```

**API:** [`DELETE /api/v1/workspaces/:slug/merges/:id`](api.md#delete-apiv1workspacesslugmergesid)
· PAT scope `merges:write`

**Behavior:** prints `{"status": "cancelled", "merge_id": "<id>"}` on stdout and
a confirmation on stderr.

**Errors:** only a `queued` job can be cancelled; the hub refuses (exit 1) a
running, finished or cancelled one.

### afc merge requeue

Queue a dead-lettered merge job again.

```
afc merge requeue <workspace-slug> <merge-id>
```

**API:** [`POST /api/v1/workspaces/:slug/merges/:id/requeue`](api.md#post-apiv1workspacesslugmergesidrequeue)
· PAT scope `merges:write`

**Behavior:** prints the job, back in `queued`.

**Errors:** the hub refuses (exit 1) a job that is not `dead_letter`.

---

## Rebase Commands

### afc rebase submit

Rebase one or more branches onto a ref and wait for the result.

```
afc rebase submit <workspace-slug> --target-ref <ref> --branches <branch1,branch2,...>
```

**API:** [`POST /api/v1/workspaces/:slug/rebase`](api.md#post-apiv1workspacesslugrebase)
with `target_ref` and `branches` · PAT scope `merges:write`

| Flag | Description |
|------|-------------|
| `--target-ref` | **Required.** Ref to rebase the branches onto |
| `--branches` | **Required.** Comma-separated branch names, sent as a JSON array |

**Behavior:** the hub rebases while the command waits and answers with a result
per branch: its new SHA, or a conflict. The command prints that answer.

**Errors:** a missing `--target-ref` or `--branches` is a usage error (exit 2).

---

## Rebuild Commands

A rebuild recreates the integration branch of a `carry_patch` workspace: the
upstream HEAD with every active patch applied in order. Syncs and pushes to
patch branches queue rebuilds by themselves (see
[`afc workspace sync`](#afc-workspace-sync)); these commands queue, inspect and
undo them by hand.

### afc rebuild submit

Queue a rebuild.

```
afc rebuild submit <workspace-slug> [--strategy <strategy>] [--fail-mode <mode>] [--wait] [--timeout <duration>] [--poll-interval <duration>]
```

**API:** [`POST /api/v1/workspaces/:slug/rebuild`](api.md#post-apiv1workspacesslugrebuild)
· PAT scope `rebuilds:write`

| Flag | Description |
|------|-------------|
| `--strategy` | `rebase` or `merge`. Default: the workspace's `rebuild_strategy` setting |
| `--fail-mode` | `fail_fast` or `continue`. Default: the workspace's `rebuild_fail_mode` setting |
| `--wait`, `--timeout`, `--poll-interval` | Wait for the job to finish; see [Waiting for a job](#waiting-for-a-job) |

**Behavior:** sends `strategy` and `fail_mode` only when the flags are given and
prints the job, with its `id` and `status` `queued`.

**Errors:** the hub refuses (exit 1) a workspace that is not in `carry_patch`
mode, one without active patches, and a duplicate job.

### afc rebuild list

List a workspace's rebuild jobs.

```
afc rebuild list <workspace-slug>
```

**API:** [`GET /api/v1/workspaces/:slug/rebuilds`](api.md#get-apiv1workspacesslugrebuilds)
· PAT scope `rebuilds:read`

**Behavior:** prints the list of jobs.

### afc rebuild status

Show one rebuild job with the result of each patch.

```
afc rebuild status <workspace-slug> <rebuild-id>
```

**API:** [`GET /api/v1/workspaces/:slug/rebuilds/:id`](api.md#get-apiv1workspacesslugrebuildsid)
· PAT scope `rebuilds:read`

**Behavior:** prints the job with its status and `patch_results`.

### afc rebuild preview

Predict, per patch, whether a rebuild would conflict, without rebuilding.

```
afc rebuild preview <workspace-slug>
```

**API:** [`GET /api/v1/workspaces/:slug/rebuild-preview`](api.md#get-apiv1workspacesslugrebuild-preview)
· PAT scope `rebuilds:read`

**Behavior:** prints the prediction for each patch against the current upstream
base.

**Errors:** the hub refuses (exit 1) a workspace that is not in `carry_patch`
mode.

### afc rebuild cancel

Cancel a queued rebuild job.

```
afc rebuild cancel <workspace-slug> <rebuild-id>
```

**API:** [`DELETE /api/v1/workspaces/:slug/rebuilds/:id`](api.md#delete-apiv1workspacesslugrebuildsid)
· PAT scope `rebuilds:write`

**Behavior:** prints `{"status": "cancelled", "rebuild_id": "<id>"}` on stdout
and a confirmation on stderr.

**Errors:** only a `queued` job can be cancelled; the hub refuses (exit 1) a
running, finished or cancelled one.

### afc rebuild requeue

Queue a dead-lettered rebuild job again.

```
afc rebuild requeue <workspace-slug> <rebuild-id>
```

**API:** [`POST /api/v1/workspaces/:slug/rebuilds/:id/requeue`](api.md#post-apiv1workspacesslugrebuildsidrequeue)
· PAT scope `rebuilds:write`

**Behavior:** prints the job, back in `queued`.

**Errors:** the hub refuses (exit 1) a job that is not `dead_letter`.

### afc rebuild rollback

Put the integration branch back to where it was before a rebuild.

```
afc rebuild rollback <workspace-slug> <rebuild-id>
```

**API:** [`POST /api/v1/workspaces/:slug/rebuilds/:id/rollback`](api.md#post-apiv1workspacesslugrebuildsidrollback)
· PAT scope `rebuilds:write`

**Behavior:** prints the result of the rollback.

**Errors:** the hub refuses (exit 1) when the rollback is not possible or the
workspace is busy.

---

## Rerere Commands

Git's rerere ("reuse recorded resolution") records how a conflict was resolved
and replays that resolution when the same conflict comes up in a later rebuild.
These commands show and drop a carry-patch workspace's recorded resolutions.

### afc rerere list

List the recorded resolutions.

```
afc rerere list <workspace-slug>
```

**API:** [`GET /api/v1/workspaces/:slug/rerere`](api.md#get-apiv1workspacesslugrerere)
· PAT scope `workspaces:read`

**Behavior:** prints the entries, each with `id` (the entry's name),
`path` (`null` unless a conflict is in progress), `recorded_at` and `resolved`
(whether there is a resolution to replay).

### afc rerere forget

Forget a recorded resolution.

```
afc rerere forget <workspace-slug> <id-or-pathspec>
```

**API:** [`DELETE /api/v1/workspaces/:slug/rerere/*pathspec`](api.md#delete-apiv1workspacesslugrererepathspec)
· PAT scope `workspaces:write`

**Behavior:**

- `<id-or-pathspec>` is an `id` from `afc rerere list`, or a file path such as
  `src/config.go`; a path works only while that file is in conflict. The slashes
  of a path are kept; every other character is percent-encoded.
- Prints `{"status": "forgotten", "pathspec": "<path>"}` on stdout and a
  confirmation on stderr.

**Errors:** the hub answers 404 (exit 1) when there is no recorded resolution
for it.

---

## Patch Commands

The patches of a `carry_patch` workspace are an ordered list of branches. A
rebuild applies the active ones, in order, on top of the upstream base. See the
[Carry-Patch Workflow](carry_patch_workflow.md) for how they fit together.

### afc patch add

Register a branch as a patch.

```
afc patch add <workspace-slug> --branch <name> [flags]
```

**API:** [`POST /api/v1/workspaces/:slug/patches`](api.md#post-apiv1workspacesslugpatches)
· PAT scope `patches:write`

| Flag | Description |
|------|-------------|
| `--branch` | **Required.** The patch's branch |
| `--position` | Position in the patch list (an integer). Default: appended |
| `--upstream-pr` | URL of the matching upstream pull request |
| `--description` | Description of the patch |
| `--skip-branch-check` | Register the patch without looking the branch up: no ref lookup, no fetch, no local branch is created. Useful before the branch has been pushed |
| `--if-not-exists` | When the branch is registered already, print the existing patch instead of failing |

**Behavior:**

- Unless `--skip-branch-check` is given, the hub looks the branch up in its
  clone before it registers it: the local branch first, then `origin`'s copy in
  the clone, and finally, with `patch_branch_source=origin`, a fetch from the
  fork.
- Prints the patch (new or, with `--if-not-exists`, existing), with `id`,
  `branch_name`, `position` and `status` (`active` for a new one).

**Errors:** a missing `--branch` is a usage error (exit 2).

### afc patch list

List a workspace's patches in position order.

```
afc patch list <workspace-slug>
```

**API:** [`GET /api/v1/workspaces/:slug/patches`](api.md#get-apiv1workspacesslugpatches)
· PAT scope `patches:read`

**Behavior:** prints a JSON array of patches, ordered by position.

### afc patch remove

Remove a patch. It can be brought back with
[`afc patch restore`](#afc-patch-restore).

```
afc patch remove <workspace-slug> <patch-id>
```

**API:** [`DELETE /api/v1/workspaces/:slug/patches/:id`](api.md#delete-apiv1workspacesslugpatchesid)
· PAT scope `patches:write`

**Behavior:** the remaining patches close the gap in the positions. Prints
`{"status": "removed", "patch_id": "<id>"}` on stdout and a confirmation on
stderr.

### afc patch update

Change a patch's status, description, upstream pull request or position.

```
afc patch update <workspace-slug> <patch-id> [flags]
```

**API:** [`PATCH /api/v1/workspaces/:slug/patches/:id`](api.md#patch-apiv1workspacesslugpatchesid)
· PAT scope `patches:write`

| Flag | Description |
|------|-------------|
| `--status` | `active`, `merged_upstream`, `conflict` or `disabled` |
| `--description` | New description |
| `--upstream-pr` | New upstream pull request URL |
| `--position` | New position in the list (an integer) |

**Behavior:** sends only the flags given and prints the updated patch.

**Errors:** no flag at all is a usage error (exit 2, nothing sent). The hub
refuses (exit 1) an invalid status and a removed patch.

### afc patch reorder

Set the order of all of a workspace's patches.

```
afc patch reorder <workspace-slug> <patch-id-1> [patch-id-2] ...
```

**API:** [`POST /api/v1/workspaces/:slug/patches/reorder`](api.md#post-apiv1workspacesslugpatchesreorder)
with `patch_ids` · PAT scope `patches:write`

**Behavior:** the IDs, at least one, give the new order. Prints the patches in
that order.

**Errors:** the hub refuses (exit 1) a list that does not name every patch of
the workspace exactly once.

### afc patch restore

Bring a removed patch back as `active`.

```
afc patch restore <workspace-slug> <patch-id>
```

**API:** [`POST /api/v1/workspaces/:slug/patches/:id/restore`](api.md#post-apiv1workspacesslugpatchesidrestore)
· PAT scope `patches:write`

**Behavior:** prints the patch with `status` `active`.

**Errors:** the hub refuses (exit 1) a patch that was not removed.

### afc patch reset-to-origin

Move a patch branch to the fork's current tip, whatever the
`patch_divergence_policy` setting says.

```
afc patch reset-to-origin <workspace-slug> <patch-id>
```

**API:** [`POST /api/v1/workspaces/:slug/patches/:id/reset-to-origin`](api.md#post-apiv1workspacesslugpatchesidreset-to-origin)
· PAT scope `patches:write`

**Behavior:**

- When the hub's tip of the branch is discarded (it diverged, or the fork was
  force-pushed), the old tip is kept under `refs/hub/replaced/<branch>`. Recover
  it with `git fetch <hub_url> refs/hub/replaced/<branch>`.
- Prints the patch, with `replaced_sha` when an old tip was kept,
  `rebuild_triggered` (boolean) and, when a rebuild was queued,
  `rebuild_job_id`.

**Errors:** the hub refuses (exit 1) a patch whose status does not allow it, an
archived workspace, and a failed fetch from the fork.

---

## Sandbox Commands

A hub offers sandboxes only when its `config.toml` has a `[sandbox]` section
(see [Server Configuration](configuration.md#sandbox)).
[`afc sandbox profiles`](#afc-sandbox-profiles) shows what the hub offers;
`create`, `list`, `get` and `destroy` work with the leases you hold (see
[Sandbox Lease Endpoints](api.md#sandbox-lease-endpoints)). A lease is the right
to one sandbox. `afc` issues and ends leases but does not start the sandbox: the
holder connects an AgentKit client to the lease's `gateway_url` with the lease
token (see [The sandbox gateway](api.md#the-sandbox-gateway)), and every lease
`afc` prints shows that address.

On a hub without a `[sandbox]` section every sandbox command fails with 404 and
the message `sandboxes are not enabled on this hub` (exit 1).

### afc sandbox profiles

List the hub's sandbox profiles: the named shapes a sandbox can take.

```
afc sandbox profiles
```

**API:** [`GET /api/v1/sandbox-profiles`](api.md#get-apiv1sandbox-profiles) ·
PAT scope `sandboxes:read` or `sandboxes:write`

**Behavior:** prints every profile, sorted by name, with its image, network,
`ttl_seconds`, resources, the names of its secrets and optional secrets, and
the keys of its fixed environment. It never shows a secret value or an `env`
value.

```
$ afc sandbox profiles
{
  "profiles": [
    {
      "default": true,
      "env_keys": [
        "LANG"
      ],
      "image": "registry.example.com/agent:1.4",
      "name": "agent",
      "network": "none",
      "optional_secrets": [],
      "require_landlock": true,
      "resources": {
        "cpus": 2,
        "disk_mb": 0,
        "memory_mb": 4096,
        "pids": 512
      },
      "secrets": [
        "ANTHROPIC_API_KEY",
        "GIT_PAT"
      ],
      "ttl_seconds": 1200
    }
  ]
}
```

### afc sandbox create

Issue a sandbox lease in a workspace.

```
afc sandbox create <workspace-slug> [--profile <name>] [--purpose <text>] [--ttl <duration>]
                    [--label <key=value>]... [--token-file <path>]
```

**API:** [`POST /api/v1/workspaces/:slug/sandboxes`](api.md#post-apiv1workspacesslugsandboxes)
· PAT scope `sandboxes:write`

| Flag | Description |
|------|-------------|
| `--profile` | Profile name. Default: the hub's default profile |
| `--purpose` | What the sandbox is for, at most 64 characters |
| `--ttl` | Lifetime of the lease, such as `30m`: at least `1m`, at most the profile's TTL. Passed to the hub as written. Default: the profile's TTL |
| `--label` | A label as `key=value`, repeatable. The value may contain `=` |
| `--token-file` | Write the lease token to this file instead of printing it |

**Behavior:**

- Sends only the fields whose flags are given.
- Without `--token-file` the command prints the whole lease, token included,
  and warns on stderr that the token will not be shown again: the hub returns
  it only in this answer. The lease's `gateway_url` is where the token is used.
- With `--token-file` the command opens the file before the request: it is
  created or truncated and set to mode `0600` whatever the umask. It then writes
  the token to the file, with no trailing newline, and prints the lease
  **without** its `token`. The token never goes to stdout.
- If writing the token fails, the command tries to end the new lease and exits
  1 with a message naming the file. The message says `the lease <id> was
  released` only when the hub confirmed it; otherwise it says the lease could
  not be released and gives the command that ends it,
  `afc sandbox destroy <slug> <id>`.
- If the hub refuses the request, the token file, truncated before the request,
  is left empty.

**Errors:** usage errors (exit 2, nothing sent): a `--label` without `=`, the
same label key twice, a `--token-file` that cannot be opened. The hub refuses
(exit 1) an unknown profile, a bad `ttl`, a profile whose secrets are missing,
a hub at capacity, and an unknown workspace.

### afc sandbox list

List a workspace's leases.

```
afc sandbox list <workspace-slug> [--all]
```

**API:** [`GET /api/v1/workspaces/:slug/sandboxes`](api.md#get-apiv1workspacesslugsandboxes)
(with `?all=true` for `--all`) · PAT scope `sandboxes:read` or `sandboxes:write`

| Flag | Description |
|------|-------------|
| `--all` | Include the leases that have ended |

**Behavior:**

- Prints the leases as one JSON array, newest first, at most 200. Without
  `--all` only live leases (`leased` or `active`) are listed; `--all` adds the
  ones that ended (`destroyed`, `expired` or `failed`) in the last 24 hours. The
  hub removes an ended lease after that (see
  [Lease lifetime](configuration.md#lease-lifetime)).
- Every lease shows its `lease_expires_at`, the deadline for its sandbox to be
  created, and, once it has ended, an `end_reason` of `client`, `api`,
  `expired`, `shutdown` or `restart`.
- The admin-only list of every workspace's leases (`GET /api/v1/sandboxes`) has
  no `afc` command.

### afc sandbox get

Show one lease.

```
afc sandbox get <workspace-slug> <lease-id>
```

**API:** [`GET /api/v1/workspaces/:slug/sandboxes/:id`](api.md#get-apiv1workspacesslugsandboxesid)
· PAT scope `sandboxes:read` or `sandboxes:write`

**Behavior:** prints the lease. It never includes the token.

**Errors:** a malformed, unknown or foreign lease ID fails with
`sandbox lease not found` (exit 1); so does a lease that ended more than 24
hours ago, since the hub has removed it.

### afc sandbox destroy

End a lease, destroy its sandbox and revoke its token.

```
afc sandbox destroy <workspace-slug> <lease-id>
```

**API:** [`DELETE /api/v1/workspaces/:slug/sandboxes/:id`](api.md#delete-apiv1workspacesslugsandboxesid)
· PAT scope `sandboxes:write`

**Behavior:**

- The hub destroys the sandbox of an `active` lease first; a lease that has no
  sandbox yet is simply ended.
- Prints `{"id": "<lease-id>", "status": "destroyed"}` on stdout and a
  confirmation on stderr. A lease that has ended already, by time or otherwise,
  is answered the same way and keeps its original `end_reason`. A lease that
  ended more than 24 hours ago is gone: `404 sandbox lease not found` (exit 1).

**Errors:** when the sandbox provider cannot destroy the sandbox, the hub answers
`502` with `error_type: sandbox_provider_error` (exit 1) and nothing changes:
the lease stays `active` and the same command can be run again. The hub gives
the provider 30 seconds, which is also how long `afc` waits for an answer, so a
provider that does not answer at all may make `afc` give up first, with exit
code 2 and no 502; run the command again.

---

## Forge Commands

The `forge`, `issue`, `pr` and `label` command groups work with a workspace's
repository on its forge (GitHub or GitLab) through the hub.
[`afc forge status`](#afc-forge-status) shows how the hub reaches the forge;
`afc issue` and `afc pr` read and write issues and pull requests; `afc label`
makes sure a label exists. [`afc forge webhook`](#afc-forge-webhook-set-unset)
and [`afc forge deliveries`](#afc-forge-deliveries) set up the forge's webhook,
so the forge tells the hub what happens on the repository, and show what it has
sent. The hub talks to the forge with the credential it holds for the
workspace, never yours, and nothing the commands print contains it (see
[Forge Endpoints](api.md#forge-endpoints)). Each command prints the hub's answer
as one JSON document.

- **The reads** (`forge status`, `forge deliveries`, `issue list`, `issue get`,
  `pr get|state|files|checks|reviews`) need an API key, an admin token, or a
  PAT with `forge:read` or `forge:write`, and work on active and archived
  workspaces. Every value is sent to the hub as given and the hub checks it: a
  bad `--state` or `--limit`, or a number that is not a positive integer, is
  answered `400` naming the parameter (exit 1). A flag that is not set is left
  out of the request.
- **The writes** (`issue create|update|close|comment|label|unlabel`,
  `label create`, `pr create|comment`, `forge webhook set|unset`) change the
  forge or the hub. They need an API key, an admin token, or a PAT with
  `forge:write` (`forge:read` gets 403), and, except `forge webhook unset`, an
  active workspace (`400 workspace is archived`). The hub records each write
  that reaches the forge as an audit event. `afc` checks what it can before it
  sends anything; see [Errors of the forge commands](#errors-of-the-forge-commands).

`--target origin|upstream` chooses the repository: `origin` (the default) or,
for a `carry_patch` workspace, its `upstream_url` repository, reached with the
upstream credential. On any other workspace `upstream` is answered `400` with
`error_type: workspace_mode_mismatch`. The flag is sent as `?target=` only when
it is given.

### Errors of the forge commands

| Code | Condition |
|------|-----------|
| 1 | The hub answered with an error: not authenticated (401); a PAT without the scope (403); a workspace that is missing or not yours, or something the forge does not have (404, the latter with `error_type: forge_not_found`); an archived workspace or a bad parameter or body (400); a body over the forge's limit (413, `error_type: body_too_large`); a forge the hub cannot identify or an invalid API URL setting (422); a forge failure (429, 502, 504); a server error. The message has the workspace's token replaced with `[redacted]` |
| 2 | Usage error, nothing sent: a wrong number of arguments; a missing required flag; `--body` with `--body-file`; a body file that cannot be read; for the writes, an issue or pull-request number that is not a positive integer; `issue update` with nothing to change; `forge webhook set` with more than one secret flag. Also no API key configured, or the hub could not be reached |

### afc forge status

Show how the hub reaches the forge of a workspace. Nothing is sent to the forge.

```
afc forge status <workspace-slug>
```

**API:** [`GET /api/v1/workspaces/:slug/forge`](api.md#get-apiv1workspacesslugforge)
· PAT scope `forge:read` or `forge:write`

**Behavior:**

- Prints `forge` (`github`, `gitlab` or `unknown`), `api_url`, `repository`
  (`host`, `owner`, `name`), `credential` (`pat`, `basic` or `none`, never a
  value), `upstream` (the same four fields for a `carry_patch` workspace, else
  `null`) and `webhook` (`configured`, `url`, `last_delivery_at`; never the
  secret).
- The `webhook` block says whether the workspace has a webhook secret, gives
  the URL to enter in the forge's webhook settings (`null` when the hub's
  `server.external_url` is not set) and when the hub last received a delivery.
- Run it when another forge command answers `forge_unknown`: it shows which
  host the hub found and whether the workspace needs a `forge_api_url` setting.

```
$ afc forge status widgets
{
  "api_url": "https://api.github.com",
  "credential": "pat",
  "forge": "github",
  "repository": {
    "host": "github.com",
    "name": "widgets",
    "owner": "acme"
  },
  "upstream": null,
  "webhook": {
    "configured": false,
    "last_delivery_at": null,
    "url": "https://hub.example.com/hooks/forge/widgets"
  }
}
```

### afc forge webhook set, unset

Set up or turn off the webhook of a workspace's forge. The forge posts what
happens on the repository (issues, labels, comments, pull requests, pushes) to
the hub, signed with a secret; the hub checks the signature, records each
delivery once and relays it as a `hub.forge.*` event, which you read on the
event stream (`GET /api/v1/events?workspace=<slug>`) or with the audit query.
The hub does not register the webhook on the forge: enter the URL and the secret
that `set` prints in the forge's webhook settings, with content type
`application/json` and these events: on GitHub, Issues, Issue comments, Pull
requests and Pushes; on GitLab, Issues, Comments, Merge requests, Push and Tag
push. See [The webhook ingress](api.md#the-webhook-ingress).

```
afc forge webhook set <workspace-slug> [--secret <s> | --from-stdin | --generate]
afc forge webhook unset <workspace-slug>
```

**API:** `PUT` and `DELETE /api/v1/workspaces/:slug/forge/webhook`
([The webhook routes](api.md#the-webhook-routes)) · PAT scope `forge:write`

| Flag (`set`) | Description |
|------|-------------|
| `--secret` | The secret, 16 to 255 printable ASCII characters (`!` to `~`). It ends up in the shell history: prefer `--from-stdin` |
| `--from-stdin` | Read the secret from stdin; one trailing newline is removed |
| `--generate` | Let the hub make a secret up. The default when none of the three is given |

**Behavior:**

- `set` sends `{"secret": ...}` for `--secret` and `--from-stdin`, `{}`
  otherwise, and prints `configured`, `url` and, when the hub made the secret
  up, `secret`. The hub shows a generated secret **once**: `set` then writes a
  note saying so on stderr, and the secret appears in no later output.
- The secret is stored as the workspace secret `FORGE_WEBHOOK_SECRET`;
  `afc secrets create FORGE_WEBHOOK_SECRET --workspace <slug>` does the same.
  Setting it again rotates it from the next delivery on.
- `unset` prints `{"status": "unset", "workspace": "<slug>"}`. The hub then
  answers the forge's deliveries with 404. It works on an archived workspace and
  when no secret was set.
- `set` needs an active workspace; `unset` does not.

**Errors:** more than one of `--secret`, `--from-stdin` and `--generate` is a
usage error (`... are mutually exclusive`, exit 2, nothing sent).

```
$ afc forge webhook set widgets
note: hub will not show this secret again; enter it in the webhook settings of the forge now
{
  "configured": true,
  "secret": "5d0c1f6e9a3b47d2b8e0f1a4c6d93e75a2b8c0d4e6f1a3b5c7d9e0f2a4b6c8d0",
  "url": "https://hub.example.com/hooks/forge/widgets"
}

$ printf '%s' "$SECRET" | afc forge webhook set widgets --from-stdin
$ afc forge webhook unset widgets
{
  "status": "unset",
  "workspace": "widgets"
}
```

### afc forge deliveries

List the webhook deliveries the hub received from a workspace's forge, newest
first, to debug a webhook.

```
afc forge deliveries <workspace-slug> [--since <time>] [--limit <n>]
```

**API:** [`GET /api/v1/workspaces/:slug/forge/deliveries`](api.md#get-apiv1workspacesslugforgedeliveries)
· PAT scope `forge:read` or `forge:write`

| Flag | Description |
|------|-------------|
| `--since` | Only deliveries received at this RFC 3339 time or later |
| `--limit` | Number of rows, 1 to 100. Default: 30 (the hub's) |

**Behavior:**

- Prints an array. A row says which delivery it was, what the hub made of it
  (`relayed`, `ignored`, `repository_mismatch` or `malformed`) and which events
  it relayed; the payload itself is not kept.
- Rows older than 7 days are deleted.

```
$ afc forge deliveries widgets --limit 2
[
  {
    "action": "labeled",
    "delivery_id": "72d3162e-cc78-11e3-81ab-4c9367dc0958",
    "event": "issues",
    "forge": "github",
    "outcome": "relayed",
    "received_at": "2026-10-21T09:12:03Z",
    "relayed": ["hub.forge.issue.labeled"],
    "target": "origin"
  }
]
```

### afc issue list

List the issues of a workspace's repository.

```
afc issue list <workspace-slug> [flags]
```

**API:** `GET /api/v1/workspaces/:slug/issues`
([The read routes](api.md#the-read-routes)) · PAT scope `forge:read` or
`forge:write`

| Flag | Description |
|------|-------------|
| `--state` | `open`, `closed` or `all`. Default: the hub's |
| `--label` | Only issues with this label; repeatable, the values are joined with commas into `labels` |
| `--assignee` | Only issues assigned to this user |
| `--limit` | Number of issues, 1 to 100. Default: 30 (the hub's) |
| `--target` | `origin` (default) or `upstream` |

**Behavior:** sends each flag that is set as a query parameter and prints the
issues as a JSON array (`[]` when there are none). Pull requests are not
included.

```
$ afc issue list widgets --label af:triage --state open --limit 5
[
  {
    "author": {
      "login": "alice"
    },
    "body": "it breaks",
    "created_at": "2026-01-02T03:04:05Z",
    "html_url": "https://github.com/acme/widgets/issues/12",
    "is_pr": false,
    "labels": [
      "af:triage"
    ],
    "number": 12,
    "state": "open",
    "title": "Widgets break",
    "updated_at": "2026-01-02T03:04:05Z"
  }
]
```

### afc issue get

Show one issue with its comments.

```
afc issue get <workspace-slug> <number> [--target origin|upstream]
```

**API:** `GET /api/v1/workspaces/:slug/issues/:n`
([The read routes](api.md#the-read-routes)) · PAT scope `forge:read` or
`forge:write`

**Behavior:** prints the issue, its `comments`, `truncated` and, when only the
comments could not be read, `comments_err`. On GitLab `<number>` is the
project's `iid`.

### afc pr get, state, files, checks, reviews

Read a pull request: the pull request itself, its state, the files it changes,
its CI checks or its reviews.

```
afc pr get     <workspace-slug> <number> [--target origin|upstream]
afc pr state   <workspace-slug> <number> [--target origin|upstream]
afc pr files   <workspace-slug> <number> [--target origin|upstream]
afc pr checks  <workspace-slug> <number> [--target origin|upstream]
afc pr reviews <workspace-slug> <number> [--target origin|upstream]
```

**API:** `GET /api/v1/workspaces/:slug/pulls/:n`, followed by `/state`,
`/files`, `/checks` or `/reviews` ([The read routes](api.md#the-read-routes)) ·
PAT scope `forge:read` or `forge:write`

**Behavior:** prints a pull request; its state (`number`, `state`, `merged`,
`head_sha`); or an array of changed files, check runs or reviews.

```
$ afc pr state fork 42 --target upstream
{
  "head_sha": "abc123",
  "merged": false,
  "number": 42,
  "state": "open"
}
```

### Body input and flags of the write commands

These rules hold for every forge write command:

- `--body <text>` gives the text on the command line; `--body-file <path>` reads
  it from a file, or from stdin when the path is `-`. The text is sent exactly
  as it is, with no trimming and the trailing newline kept (unlike a secret read
  from stdin, a body is a document). `--body` and `--body-file` together are a
  usage error.
- `--label` and `--assignee` are repeatable and take one name each: a comma is
  part of the name, so `--label a,b` is the single label `a,b`.
- A flag that is not set is not sent. `afc issue update w 3 --body ""` sends
  `{"body":""}`, which clears the body.
- `--target upstream` writes to the upstream repository of a `carry_patch`
  workspace, with the upstream credential.
- A body-like field over the forge's limit (65,536 characters on GitHub,
  1,000,000 on GitLab, counted in Unicode code points) is refused by the hub
  with `413` and `error_type: body_too_large`, and nothing is sent to the forge.

### afc issue create

Create an issue.

```
afc issue create <workspace-slug> --title <title> [--body <text> | --body-file <path>]
                 [--label <name>]... [--assignee <user>]... [--target origin|upstream]
```

**API:** `POST /api/v1/workspaces/:slug/issues`
([The write routes](api.md#the-write-routes)) · PAT scope `forge:write`

| Flag | Description |
|------|-------------|
| `--title` | **Required.** Title of the issue |
| `--body`, `--body-file` | Body of the issue; see [Body input](#body-input-and-flags-of-the-write-commands) |
| `--label` | Label to put on the issue; repeatable |
| `--assignee` | User to assign; repeatable |
| `--target` | `origin` (default) or `upstream` |

**Behavior:**

- Prints the created issue (201).
- On GitLab the assignees are accepted but not sent (GitLab takes user ids, not
  names).

```
$ afc issue create widgets --title "Widgets break" --body-file report.md --label af:triage
{
  "author": {"login": "agent-fox"},
  "html_url": "https://github.com/acme/widgets/issues/13",
  "labels": ["af:triage"],
  "number": 13,
  "state": "open",
  "title": "Widgets break"
}
```

### afc issue update

Change an issue. Only the fields given change.

```
afc issue update <workspace-slug> <number> [--title <title>] [--body <text> | --body-file <path>]
                 [--state open|closed] [--label <name>]... [--assignee <user>]...
                 [--target origin|upstream]
```

**API:** `PATCH /api/v1/workspaces/:slug/issues/:n`
([The write routes](api.md#the-write-routes)) · PAT scope `forge:write`

| Flag | Description |
|------|-------------|
| `--title` | New title |
| `--body`, `--body-file` | New body; an empty `--body` clears it |
| `--state` | `open` or `closed` |
| `--label` | Label to set; repeatable. The labels given **replace** the issue's whole set |
| `--assignee` | User to assign; repeatable. The users given **replace** the issue's whole set |
| `--target` | `origin` (default) or `upstream` |

**Behavior:**

- Prints the issue as the forge returns it.
- `--label` and `--assignee` do not add to the issue's labels or assignees. To
  remove every label, use the hub's API with `"labels": []`; to add a label and
  keep the others, use [`afc issue label`](#afc-issue-label-unlabel).
- On GitLab `<number>` is the project's `iid`, and `--assignee` is not sent (the
  hub answers `400`, `nothing to update`, when it is the only change).

**Errors:** without any of `--title`, `--body`, `--body-file`, `--state`,
`--label` and `--assignee` the command exits 2 (`nothing to update`).

### afc issue close

Close an issue, with a comment if given.

```
afc issue close <workspace-slug> <number> [--comment <text>] [--target origin|upstream]
```

**API:** `POST /api/v1/workspaces/:slug/issues/:n/close`
([The write routes](api.md#the-write-routes)) · PAT scope `forge:write`

| Flag | Description |
|------|-------------|
| `--comment` | Comment to post on the issue before it is closed |
| `--target` | `origin` (default) or `upstream` |

**Behavior:**

- Sends `{"comment": ...}` when `--comment` is given and prints the closed
  issue.
- With a comment that is not blank, the hub posts the comment first and closes
  the issue only if that worked; a comment the forge refuses leaves the issue
  open. The hub records one `hub.forge.issue.close` event, which holds the
  comment's URL.
- Closing an issue that is closed already succeeds.

### afc issue comment

Comment on an issue.

```
afc issue comment <workspace-slug> <number> (--body <text> | --body-file <path>)
                  [--target origin|upstream]
```

**API:** `POST /api/v1/workspaces/:slug/issues/:n/comments`
([The write routes](api.md#the-write-routes)) · PAT scope `forge:write`

**Behavior:** prints `{"url": "<the comment's URL>"}` (201).

**Errors:** a missing `--body`/`--body-file` is a usage error (exit 2).

### afc issue label, unlabel

Add labels to an issue, or remove one.

```
afc issue label   <workspace-slug> <number> <label>... [--target origin|upstream]
afc issue unlabel <workspace-slug> <number> <label>    [--target origin|upstream]
```

**API:** `POST /api/v1/workspaces/:slug/issues/:n/labels` and
`DELETE /api/v1/workspaces/:slug/issues/:n/labels/:name`
([The write routes](api.md#the-write-routes)) · PAT scope `forge:write`

**Behavior:**

- `label` sends the labels, one to 100, as a bare JSON array. The issue keeps
  its other labels. A label the repository does not have is the forge's
  business (GitHub creates it).
- `unlabel` sends the label as one path segment (`type/bug` becomes
  `type%2Fbug`). It succeeds also when the issue never had the label, on GitHub
  and on GitLab; when the issue does not exist it fails with `404` and
  `error_type: forge_not_found`.
- The hub answers 204, so the commands print a status document:

```
$ afc issue label widgets 3 af:ready af:fix
{"status": "labeled", "number": 3, "labels": ["af:ready", "af:fix"]}
$ afc issue unlabel widgets 3 type/bug
{"status": "unlabeled", "number": 3, "label": "type/bug"}
```

### afc label create

Make sure a label exists in the workspace's repository.

```
afc label create <workspace-slug> <name> [--color <hex>] [--description <text>]
                 [--target origin|upstream]
```

**API:** `POST /api/v1/workspaces/:slug/labels`
([The write routes](api.md#the-write-routes)) · PAT scope `forge:write`

| Flag | Description |
|------|-------------|
| `--color` | Six hex digits, with or without a leading `#`. Default: `ededed` |
| `--description` | Description of the label |
| `--target` | `origin` (default) or `upstream` |

**Behavior:** a label that exists is printed as the forge holds it (200) and is
**never changed**, whatever `--color` and `--description` say; a missing label
is created and printed (201). Both exit 0, so the command is safe to repeat.

**Errors:** an empty name is a usage error (exit 2).

### afc pr create

Open a pull request (a merge request on GitLab).

```
afc pr create <workspace-slug> --title <title> --head <branch> --base <branch>
              [--body <text> | --body-file <path>] [--draft] [--target origin|upstream]
```

**API:** `POST /api/v1/workspaces/:slug/pulls`
([The write routes](api.md#the-write-routes)) · PAT scope `forge:write`

| Flag | Description |
|------|-------------|
| `--title` | **Required.** Title of the pull request |
| `--head` | **Required.** Branch with the changes; `owner:branch` for a fork |
| `--base` | **Required.** Branch to merge into |
| `--body`, `--body-file` | Body of the pull request |
| `--draft` | Open the pull request as a draft |
| `--target` | `origin` (default) or `upstream` |

**Behavior:**

- Prints the pull request (201).
- `--head` is sent as given, so GitHub's `owner:branch` opens a pull request
  from a fork. With `--target upstream` on a `carry_patch` workspace the pull
  request is opened on the upstream repository.
- The hub does not check for duplicates: a pull request that exists already for
  the head and base is refused by the forge (exit 1,
  `error_type: forge_rejected`).

### afc pr comment

Comment on the conversation of a pull request.

```
afc pr comment <workspace-slug> <number> (--body <text> | --body-file <path>)
               [--target origin|upstream]
```

**API:** `POST /api/v1/workspaces/:slug/pulls/:n/comments`
([The write routes](api.md#the-write-routes)) · PAT scope `forge:write`

**Behavior:** prints `{"url": "<the comment's URL>"}` (201).

**Errors:** a missing `--body`/`--body-file` is a usage error (exit 2).

---

## Account and Admin Commands

These commands manage your sign-in, profile, API keys, personal access tokens
and organizations, and, for admins, the hub's users and organizations. See
[Identity and Access Endpoints](api.md#identity-and-access-endpoints-apikit-provided)
for the requests and the [Permissions Reference](permissions.md) for keys,
tokens and scopes.

### afc login

Sign in to the hub and store the credential in the config file.

```
afc login [--endpoint-url <url>] [--provider <name>] [--expires <days>]
afc login [--endpoint-url <url>] --api-key <key | ->
```

**API:** [`GET /api/v1/auth/providers`](api.md#get-apiv1authproviders) and
[`POST /api/v1/auth/callback`](api.md#post-apiv1authcallback) for a browser
login; [`GET /api/v1/user`](api.md#get-apiv1user) to check a key given with
`--api-key`. No credential is needed.

| Flag | Description |
|------|-------------|
| `--endpoint-url` | The hub's URL. Default: `AF_ENDPOINT_URL`, `ENDPOINT_URL`, then the config file. Required from one of them |
| `--provider` | OAuth provider to sign in with, as the hub lists it. Default: `github` |
| `--expires` | Lifetime of the new API key in days: `0` (never), `30`, `60` or `90`. Default: `90` |
| `--api-key` | Sign in without a browser with an existing API key or PAT; `-` reads it from stdin |

**Behavior:**

- **Browser login** (no `--api-key`): asks the hub for its OAuth providers,
  opens the chosen provider's sign-in page in the browser (stderr says
  `Opening browser for authentication...`, or prints the URL to open when no
  browser can be started) and waits up to 120 seconds for the provider to
  redirect back to a one-off listener on `127.0.0.1`. It then exchanges the code
  for a new API key, which revokes every older API key of your user, so
  automation that still holds one stops working.
- **Key login** (`--api-key`): checks the key against the hub and stores it; no
  new key is made and no key is revoked.
- Either way the command saves `endpoint_url`, `user_id` and `api_key` to
  `~/.af/config.toml` (mode `0600`), prints your user as JSON on stdout and
  `Logged in as <username>` on stderr.

**Errors:** no hub URL from any source, an `--expires` other than 0, 30, 60 or
90, a provider the hub does not list, an OAuth state mismatch, no browser
callback within 120 seconds, an empty key from stdin, or a refusal by the hub
(its message is shown). Unlike the other commands, `afc login` exits 2 for all of
them, a refusal by the hub included.

### afc user

Show or change your user profile.

```
afc user show
afc user update --full-name <name>
```

**API:** [`GET /api/v1/user`](api.md#get-apiv1user) and
[`PATCH /api/v1/user`](api.md#patch-apiv1user) · PAT scope `users:read`

| Flag (`update`) | Description |
|------|-------------|
| `--full-name` | **Required.** Your display name |

**Behavior:** `show` prints your profile; `update` sets your display name and
prints the updated profile.

### afc keys

Manage your API keys.

```
afc keys <list | refresh | revoke> [flags]
```

| Subcommand | Description |
|------------|-------------|
| `list` | List your API keys |
| `refresh` | Rotate the current key: a new secret under the same `key_id`, saved to the config file |
| `revoke` | Revoke the current key at once and remove it from the config file |

### afc tokens

Manage personal access tokens (PATs).

```
afc tokens <subcommand> [flags]
```

| Subcommand | Description |
|------------|-------------|
| `list` | List your PATs with their permissions |
| `create` | Create a PAT. Its value is shown only in this output |
| `show <token_id>` | Show a PAT |
| `revoke <token_id>` | Revoke a PAT at once |
| `replace <token_id>` | Replace a PAT's whole permission set |
| `add <token_id>` | Add permissions to a PAT |
| `remove <token_id>` | Remove permissions from a PAT without revoking it |

| Flag | Subcommands | Description |
|------|-------------|-------------|
| `--name` | `create` | **Required.** Name of the token |
| `--permissions` | `create`, `replace`, `add`, `remove` | **Required.** Comma-separated permissions, for example `users:read,orgs:read` |
| `--expires` | `create` | Lifetime in days: `0` (never), `30`, `60` or `90`. Default: `90` |
| `--workspace` | `create`, `list` | Work with tokens bound to this workspace; see below |

**Workspace-bound tokens.** A token bound to a workspace works only on that
workspace (see [Workspace-Bound Tokens](api.md#workspace-bound-tokens)). Without
`--workspace`, `create` and `list` behave as described above.

- `create --workspace <slug>` calls
  [`POST /api/v1/workspaces/:slug/tokens`](api.md#post-apiv1workspacesslugtokens)
  with `name`, `permissions` (split on commas, trimmed, empty entries dropped)
  and `expires`. It prints the answer, the only place the token is shown, as one
  JSON document and writes
  `warning: the token in this output will not be shown again; save it now` on
  stderr.
- `list --workspace <slug>` calls
  [`GET /api/v1/workspaces/:slug/tokens`](api.md#get-apiv1workspacesslugtokens)
  and prints every token bound to the workspace, revoked and expired ones
  included, without any secret.
- An empty `--workspace` or `--name`, or a `--permissions` with no entry left,
  is a usage error (exit 2, nothing sent).

```
afc tokens create --workspace my-ws --name agent-fox --permissions workspaces:read,patches:read
afc tokens list --workspace my-ws
```

### afc orgs

Show the organizations you belong to.

```
afc orgs <subcommand> [flags]
```

| Subcommand | Description |
|------------|-------------|
| `list` | List your organizations |
| `show <id\|slug>` | Show an organization, by UUID or slug |
| `members <id\|slug>` | List an organization's members |

### afc admin

Manage the hub's users, organizations, API keys and tokens. Every subcommand
needs an admin credential; any other gets 403 (exit 1).

```
afc admin <users | orgs | keys | tokens> <subcommand> [flags]
```

**`afc admin users`**

| Subcommand | Description |
|------------|-------------|
| `list` | List all users |
| `show <id\|username\|email>` | Show a user |
| `create` | Create a user |
| `update <id\|username\|email>` | Update a user |
| `promote <id\|username\|email>` | Give a user the admin role |
| `demote <id\|username\|email>` | Take the admin role away |
| `block <id\|username\|email>` | Block a user |
| `unblock <id\|username\|email>` | Unblock a user |

**`afc admin orgs`**

| Subcommand | Description |
|------------|-------------|
| `list` | List all organizations |
| `create` | Create an organization |
| `update <id\|slug>` | Update an organization |
| `delete <id\|slug>` | Delete an organization |
| `block <id\|slug>` | Block an organization |
| `unblock <id\|slug>` | Unblock an organization |
| `members list <id\|slug>` | List an organization's members |
| `members add <org> <user>` | Add a member |
| `members remove <org> <user>` | Remove a member |

**`afc admin keys`**

| Subcommand | Description |
|------------|-------------|
| `list <user_id\|username\|email>` | List a user's API keys |
| `revoke <user_id\|username\|email> <key_id>` | Revoke a user's API key |

**`afc admin tokens`**

| Subcommand | Description |
|------------|-------------|
| `list <user_id\|username\|email>` | List a user's personal access tokens |
| `revoke <user_id\|username\|email> <token_id>` | Revoke a user's personal access token |
