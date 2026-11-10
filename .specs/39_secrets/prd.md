---
spec_id: "39"
spec_name: "secrets"
title: "Store secrets that can be written but never read"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "ff396ecc0bc25fb4b1fd9f0358492b50d8613ed9c1137cadd79399119a232df7"
schema_version: 3
source: "docs/prd/11_secrets_and_variables.md"
---

# Store secrets that can be written but never read

## Intent

The hub keeps named secrets for a user, an organization and a workspace so that it can act with them on the owner's behalf: clone and push with a git credential, call a forge, verify a webhook, give a sandbox a credential. A secret is write-only: once stored, its value leaves the hub through no response, event, log line or error message, and it lies in the hub's storage only under authenticated encryption with a key the operator supplies and can rotate. A copy of the hub's storage therefore discloses no secret, and the people who store secrets can trust that nobody, themselves included, reads them back.

## Goals

- A caller creates, lists, updates and deletes secrets in three scopes: their own user (`/user/secrets`), an organization they belong to (`/orgs/:slug/secrets`) and a workspace they own (`/workspaces/:slug/secrets`).
- No route returns a secret value. A list carries `key`, `created_at` and `updated_at` and nothing else.
- Every stored value is encrypted with AES-256-GCM under the operator's key, and each stored value records which key encrypted it.
- Without a key the hub starts, lists and deletes secrets, and refuses to store one with `503 secrets_unavailable`.
- An operator rotates the key by setting the old one as `PARLEY_SECRETS_KEY_PREVIOUS`; the hourly maintenance pass re-encrypts every value and reports how many remain.
- A scope holds at most 100 secrets; a key has 1 to 255 characters; a value has at most 262,144 bytes.
- Every created entry, update and delete writes one event without the value, in the transaction of the change.
- Other parts of the hub read and store a named secret of one scope through one internal component, and remove a scope's secrets inside their own transaction.
- `parley secrets create|list|update|delete` drives the routes from the shell.

## Non-goals

- Variables, their routes, `GET /workspaces/:slug/vars/resolved`, the reservation of `resolved` (SEC-FR-015, SEC-FR-017), the resolution chain of §11.3 for variables and for secrets (the parent step, the owner test, reserved names skipped on the parent) and `parley vars …`: package 40 variables_and_resolution. This package reads a secret from exactly one scope.
- What the hub does with a reserved name: origin and upstream credential resolution from `GIT_PAT`, `GIT_USERNAME`, `GIT_PASSWORD` and the `UPSTREAM_…` names (packages 10 workspace_create_and_clone and 31 carry_patch_setup_and_patches), the forge token (package 41 forge_reads), `FORGE_WEBHOOK_SECRET` and the fact that deleting it disables the webhook (package 43 forge_webhook_ingress). This package blocks no name and gives none a meaning.
- Credentials of a sandbox and exposed secrets: package 45 sandbox_creation_and_credentials.
- The signing secret of an event webhook: package 53 event_webhooks, which seals it with this package's cipher and registers a converter for rotation.
- The answers a workspace-bound token gets (SEC-FR-002: `403 token_bound` on `/user/…` and `/orgs/…` routes and on a fork's parent, `404` on another workspace): the binding guard of package 08 workspace_bound_tokens, which runs before this package's handlers.
- The error envelope, the order of checks, list paging, `Idempotency-Key`, body size and content type: package 02 http_api_conventions. The scope registry and the PAT scope check: package 06 api_keys_and_access_tokens. The workspace access rule: package 07 workspace_access_rule.
- Who sees a secret event in `/events` and `/audit` (§11.8, §14.3.7): package 52 durable_event_log_and_stream. This package writes the event with the fields that rule reads.
- The maintenance loop, the configuration loader, the scrubber and the logger: package 03 server_lifecycle_and_configuration.
- Running the workspace remover when a workspace is deleted or a fork expires: packages 14 workspace_archive_reclone_delete and 16 task_fork_lifetime. This package supplies the remover.
- The CLI's client, output and exit codes: package 09 cli_foundation.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/11_secrets_and_variables.md`: the secret half of SEC-FR-001 to SEC-FR-017, with §11.3 (scopes, entry, limits, encryption), §11.5 (the twelve secret routes), §11.6 (`parley secrets`), §11.7 (start report and re-encryption), §11.8 (events), §11.9 (configuration) and §11.10.

The code lives in `internal/secret/`:

| File | Holds |
|---|---|
| `keys.go` | loading and checking the current and the previous key |
| `cipher.go` | sealing and opening a value, the key id |
| `store.go` | the secret table and the internal component other packages call |
| `handlers.go` | the twelve routes, scope resolution and access |
| `reencrypt.go` | the maintenance task and the converter registry |
| `report.go` | the start report |

Each has its test file beside it; `internal/secret/secret_integration_test.go` holds the end-to-end tests, which build the real router, store, identity, access and event components in-process and drive the CLI's root command against them. The commands live in `internal/cli/secrets.go`. The process composition in `internal/hub/` wires the component, and `internal/identity/` gains a hook this package adds (Design Decision 15).

What this package builds on: the store, schema steps, the configuration key registry, start hooks, the maintenance task registry and the scrubber of package 03; route registration, the error envelope and the list helper of package 02; the authenticated caller of package 04; users, organizations and memberships of package 05; the workspace record and the access decision of package 07; the event component of package 52; the CLI foundation of package 09.

Packages 10, 41, 43, 45 and 53 depend on this one for the internal component.

## Requirements

### Scopes and the entry

A secret belongs to one scope (§11.3):

| Scope | Route prefix | `scope_id` |
|---|---|---|
| `user` | `/user` | the caller's user id |
| `org` | `/orgs/:slug` | the organization id |
| `workspace` | `/workspaces/:slug` | the workspace slug |

A task fork is a workspace and has its own `workspace` scope. A workspace's secret routes show and change only that workspace's own secrets, never those of a parent or a fork.

An entry has a `key`, a value, `created_at` and `updated_at` (equal on create). The key is stored and returned with the casing given at create, and matched without regard to ASCII case for uniqueness, update, delete and reading. Responses carry `{key, created_at, updated_at}`: never the value and never the owner.

Limits: a key is 1 to 255 characters from ASCII letters, digits and `_`, and does not start with a digit; a value is at most 262,144 bytes, counted in bytes, and may be empty; a scope holds at most 100 secrets. Any valid key may be created; the hub blocks no name.

### Access (SEC-FR-001, SEC-FR-004 to SEC-FR-006)

Every route follows the global check order. A PAT needs `secrets:read` on the list route (which `secrets:write` also satisfies) and `secrets:write` on create, update and delete; a PAT without it gets `403 insufficient_scope` whether or not the org or workspace exists. API keys and the admin token are not scope-checked.

- **User scope.** Any credential with a user identity. The admin token has no user and gets `403 forbidden` with the message `user-scoped endpoints require a user credential`.
- **Org scope.** Members of the org and the admin token. An unknown org and a caller who is not a member get `404 not_found` with the same body.
- **Workspace scope.** The routes have need `owner` under the workspace access rule (PLAT-FR-060): the owner (for a fork also the owner of its parent) and the admin token are allowed; an org member gets `403 forbidden` with the message `org members have read-only access to this workspace` on every secret route, the list included; anyone else, and an unknown slug, get `404 not_found` with the message `workspace not found`. The owner of a fork who is only a member of the parent gets that `403` on the parent's secret routes. The workspace's status does not matter and an exclusive maintenance operation does not block these routes.

### Create (SEC-FR-007)

`POST /<scope>/secrets` takes `{"entries":[{"key": string, "value": string}, …]}` and is all-or-nothing. A missing `value` is the empty string. The checks run in this order, and the first that fails decides:

1. `entries` is a non-empty array; each key is valid; each value is within the size limit; no two keys of the request are equal ignoring case. A violation is `400 invalid_request`, and the message names the key where there is one.
2. An encryption key is configured, else `503 secrets_unavailable` (SEC-FR-011).
3. The scope's count plus the new entries is at most 100, else `429` with `error_type` `secret_limit_reached` and the message `maximum of 100 entries per scope exceeded`. A full scope answers this even when a key of the request also conflicts (§11.10).
4. No key is already present ignoring case, else `409 conflict` with the message `key "<k>" already exists`.

On success the answer is `201` with `{"items":[{key, created_at, updated_at}, …]}` in request order; all entries carry the same `created_at`. The route accepts `Idempotency-Key`.

### List (SEC-FR-010)

`GET /<scope>/secrets` answers `200` with `{"items":[…],"next_cursor":…}`, ordered by key ascending without regard to case, paged by the list convention. It never carries a value. It works without an encryption key and lists entries that cannot be decrypted (SEC-FR-011, SEC-FR-013).

### Update and delete (SEC-FR-008, SEC-FR-009, SEC-FR-013)

`PATCH /<scope>/secrets/:key` takes `{"value": string}`. An absent or `null` value, a value that is not a string and a value over the size limit are `400 invalid_request`; `""` is valid. Without an encryption key the answer is `503 secrets_unavailable`. The key is matched ignoring case; no match is `404 not_found` with the message `key "<k>" not found`. The stored key casing and `created_at` are kept and `updated_at` becomes now. The answer is `200` with the entry. An entry that cannot be decrypted can be overwritten this way.

`DELETE /<scope>/secrets/:key` matches the key ignoring case. No match is `404 not_found` with the same message. Success is `204` with no body. Delete works without an encryption key and on an entry that cannot be decrypted.

### Write-only values and events (SEC-FR-003, SEC-FR-016, §11.8)

A secret value appears in no response, event, log line or error message. Only hub code reads it, through the internal component.

Each created entry, each update and each delete writes one event in the transaction of the change; a failed request writes none, and a change whose event cannot be written is not made (`500 internal`).

| Event type | `resource_type` | `action` | When |
|---|---|---|---|
| `parley.secret.create` | `secret` | `create` | per entry of a successful create |
| `parley.secret.update` | `secret` | `update` | successful update |
| `parley.secret.delete` | `secret` | `delete` | successful delete |

Every event carries `resource_id` = the stored key and `metadata` `{"scope": "user"|"org"|"workspace", "scope_id": <user id, org id or workspace slug>, "key": <stored key>}`. For the workspace scope the event's `workspace` is the slug; otherwise it is null. Secrets that other routes store through the internal component write no secret event.

### Encryption and keys (SEC-FR-011 to SEC-FR-013, §11.7, §11.9)

| Key | Default | Meaning |
|---|---|---|
| `PARLEY_SECRETS_KEY` (environment) | unset | current key: 32 random bytes, base64-encoded |
| `[secrets].key_file` | unset | path of a file holding the same key; the environment variable wins when both are set |
| `PARLEY_SECRETS_KEY_PREVIOUS` (environment) | unset | former key, accepted for decryption only |

A malformed key stops the start with an error. The hub never generates, stores or prints a key. New and updated values always use the current key. A stored value whose key matches neither configured key, or that fails authentication, stays listed, can be overwritten or deleted, and fails every operation that needs its value with an error that names the key and not the value; such a failure is never treated as "no secret".

At start the hub logs the state of secret encryption: whether a key is configured, whether a previous key is present, and how many stored secrets cannot be decrypted with the configured keys. Without a key it also logs a warning that secrets cannot be stored.

### Re-encryption (SEC-FR-012, §11.7)

The hourly maintenance pass runs the task `secret_reencryption`: every secret still encrypted under the previous key is re-encrypted under the current key. The pass does not change `updated_at`, writes no events and logs how many values it converted and how many remain. To rotate, an operator sets the new key as `PARLEY_SECRETS_KEY` and the old one as `PARLEY_SECRETS_KEY_PREVIOUS`, waits for the pass to report zero remaining, then removes the previous key.

### The internal component

Other packages read a named secret of one scope (`Get`), store one inside their own transaction (`Put`: workspace creation with git credentials in package 10, the forge webhook secret in package 43), delete one named secret inside their own transaction (`Delete`: package 43 when the forge webhook secret is unset), ask whether a key is configured so they can answer `503 secrets_unavailable` (`Available`), and remove every secret of a scope inside their own transaction (`RemoveScope`). None of these writes a secret event. A read goes to the store on every call and is never cached, so a rotated secret applies to the next operation (§11.3). Deleting a workspace, an org or a user deletes that scope's secrets in the same transaction, and a workspace created again under the same slug starts empty (SEC-FR-014).

### CLI (§11.6)

| Command | Calls | Prints |
|---|---|---|
| `parley secrets create <KEY=VALUE[,KEY=VALUE…]>… \| <KEY> --from-stdin` | `POST /<scope>/secrets`, once per scope flag | `{"results":[{"scope","items":[…]}, …]}` |
| `parley secrets list` | `GET /<scope>/secrets` with `limit=200`, all pages | `{"items":[…]}` |
| `parley secrets update <KEY=VALUE> \| <KEY> --from-stdin` | `PATCH /<scope>/secrets/<KEY>` | the updated entry |
| `parley secrets delete <KEY>` | `DELETE /<scope>/secrets/<KEY>` | `{"deleted":"<KEY>"}` |

Scope flags are `--user`, `--org <slug>` and `--workspace <slug>`; without a flag the scope is the user. `list`, `update` and `delete` accept at most one. `create` accepts several: one request is sent per scope in the order user, org, workspace; a failing scope does not stop the others; the document on stdout holds the scopes that succeeded; each failed scope's error envelope is printed once on stderr and the command exits 1 if any scope failed.

`KEY=VALUE` is split at the first `=`, so a value may contain `=`. A comma separates entries, so a value with a comma needs `--from-stdin`, which takes one bare `KEY` and reads the value from standard input, dropping one trailing line ending. Key naming rules are checked by the hub. Malformed arguments are usage errors (exit 2) and nothing is sent.

### End-to-end flows

1. **A developer keeps a workspace secret.** A developer runs `parley secrets create GIT_PAT=… --workspace acme`; the parley CLI posts to `parleyd`, which checks that the developer owns the workspace, encrypts the value, stores it and writes `parley.secret.create`. `parley secrets list --workspace acme` shows the key without a value. The developer updates and then deletes the secret; hub code that reads the secret between those steps gets the new value. The value is in no output, event or log line, and the database file does not contain it.
2. **An operator rotates the key.** An operator restarts `parleyd` with a new `PARLEY_SECRETS_KEY` and the old key as `PARLEY_SECRETS_KEY_PREVIOUS`. The start report shows a previous key and no undecryptable secret. The maintenance pass re-encrypts every value and logs zero remaining. The operator restarts without the previous key; every secret is still readable by the hub and `updated_at` has not changed.
3. **An org shares a secret and a member is kept out of a workspace's secrets.** An org member stores a secret in the org's scope and another member lists it. The same member asks for the secrets of a workspace of that org that a developer owns and gets `403 forbidden`; a user outside the org gets `404 not_found` for the org's and for the workspace's secrets.

## Design Decisions

1. **Project decisions relied on.** Go; the store is embedded SQLite through `modernc.org/sqlite` behind `internal/store` (project decision 2); HTTP uses `net/http` and the route registration of package 02 (decision 3); the CLI uses `github.com/spf13/cobra` on the foundation of package 09 (decision 4); tests use `testing`, table-driven, with `net/http/httptest` (decision 7); code takes a replaceable clock.
2. **Algorithm.** AES-256-GCM from the standard library (`crypto/aes`, `crypto/cipher`), a fresh 12-byte nonce from `crypto/rand` for each sealing. The PRD says "authenticated encryption" and names none; this needs no module.
3. **Key id.** Each stored value records the first 8 bytes, in lowercase hex, of the SHA-256 of the key that sealed it. Which key opens a value, and whether a value is "undecryptable", is decided by this id; the count in the start report and `remaining` in the maintenance log compare ids and decrypt nothing.
4. **A value is bound to its row.** The additional authenticated data is the scope, the scope id and the lowercase key, so a ciphertext copied to another row fails authentication.
5. **Key encoding.** Standard base64 with padding (`openssl rand -base64 32`), surrounding white space ignored, decoding to exactly 32 bytes. An environment variable that is set and empty counts as unset. `[secrets].key_file` is read only when `PARLEY_SECRETS_KEY` is unset; a named file that cannot be read stops the start. A start error exits with status 1 and names the variable or the file, never the value.
6. **A previous key without a current key is accepted.** Values under it are read; storing answers `503`. The PRD is silent; the keys are independent settings.
7. **Where `503` sits in the order.** The key check is a state precondition: it comes after request validation and before the limit, the conflict and the lookup of the entry. A `PATCH` of an unknown key without an encryption key is therefore `503`.
8. **Create details the PRD leaves open.** A `value` that is present and not a JSON string (`null` included) is `400`. A key or value check that fails names the first failing entry in request order; the `409` message names the first conflicting key in request order, as the request spelled it. Uniqueness is enforced by the store, so of two concurrent creates of one key exactly one succeeds.
9. **A key in a path is not validated.** A `PATCH` or `DELETE` whose `:key` is not a legal key matches nothing and is `404 not_found`.
10. **Case folding is ASCII.** Keys are ASCII, so matching and ordering compare the keys with `A`–`Z` lowered, byte by byte.
11. **Organization in the path.** `:slug` is resolved with the organization selector of package 05 (a slug, or a UUID). A blocked org is answered to the admin token only; a member gets the `404` of a non-member, as package 05 decided for `GET /orgs/:id`.
12. **The admin token on user routes** is answered by this package at the access step with the message of SEC-FR-004, not by the "needs a user" declaration of package 04, whose message differs. Status and `error_type` are the same.
13. **The internal store operation `Put` is an upsert under the public rules.** It checks the key, the value size and the limit of 100 (when it would add an entry), and returns typed errors: invalid, limit reached, unavailable, unreadable. An entry that matches ignoring case is overwritten and keeps its stored casing. It writes no event; the calling route writes its own. `Delete` removes one named entry inside the caller's transaction, reports whether one existed and writes no event. Packages 10, 41, 43, 45 and 53 use these operations under these names (`Get`, `Put`, `Delete`, `Available`, `RemoveScope`); package 40 reads through `Get`.
14. **Decrypted values are registered with the scrubber** of package 03 when the internal read returns them, so text that leaves the hub later cannot carry them.
15. **Removing a scope.** The component exports `RemoveScope(tx, scope, scope id)`, which deletes every secret of a scope inside a transaction it is given. *Users and organizations (with package 05):* package 05 owns `internal/identity/removers.go` with `RegisterUserRemover` and `RegisterOrgRemover`, which take a function of a transaction and an id, and its delete of a user and of an org call every registered function inside their transaction, before the record is removed. This package only registers: `internal/hub` registers `RemoveScope` for the two scopes through those two functions, and this package edits no file of `internal/identity`. Package 40 registers its variable removal with the same two functions; no other mechanism removes a deleted owner's entries. *Workspaces and forks:* this package exports `RemoveWorkspace(tx, slug)`, which deletes every secret of that workspace scope inside the transaction it is given, writes no event and returns the store's error. Package 14's delete runs it as a remover of owned data (a function of the transaction and one slug that returns the ids of the tokens it revoked; this one returns none). That registry is created after this package (14 depends on 10, and 10 on this one), so the registration in `internal/hub` is written with package 14. A fork's secrets are removed by package 14's `RemoveOwned`, which runs `secret.RemoveWorkspace`; package 16 makes no call of its own.
16. **Converters.** Package 53 seals the signing secret of an event webhook with the same key and outside the secret table. The re-encryption task therefore accepts registered converters (a function that re-seals its owner's values and returns the numbers converted and remaining), so that "zero remaining" covers every value under the previous key.
17. **Re-encryption details.** A value that fails to open under the previous key is left as it is and counted as remaining; the task still succeeds. A value changed between the task's read and its write is left as the newer write made it. A store failure fails the task.
18. **CLI output.** `list` reads the pages with `limit=200` and prints one document `{"items":[…]}` holding the items of every page in order, without `next_cursor`; `parley vars list` and `parley vars resolve` of package 40 print the same shape. `create` with every scope failing prints `{"results":[]}`. `--user` together with another scope flag is allowed on `create` only. Several positional arguments of `create` are joined as if separated by commas.
19. **Dependencies beyond the plan's list.** 09 (the commands are built on the CLI foundation) and 52 (the events are written through the event component), as the brief's notes require.
20. **Profile: full**, with three execution paths.

## Dependencies

- Package 05 users_and_organizations (draft): users, organizations, memberships, the organization selector, and the delete of a user and of an org that this package hooks. Through it packages 02, 03 and 04.
- Package 07 workspace_access_rule (draft): the workspace record and the access decision with need `owner`. Through it package 06 (the scope check).
- Package 09 cli_foundation (draft): the command helper, HTTP client, output and exit codes.
- Package 52 durable_event_log_and_stream (draft): writing an event in the transaction of its state change.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number. Hub events form the audit log and the event stream.
- **Secret scope**: The owner of a secret: `user`, `org` or `workspace`, with the id of that owner. (The word "scope" alone also names a PAT permission; this package says "secret scope" or "PAT scope" where the two could be confused.)
- **Current key, previous key**: The operator's encryption key for new values, and the former key the hub still accepts for decryption during a rotation.
- **Key id**: The short fingerprint, stored with each value, of the key that encrypted it.
