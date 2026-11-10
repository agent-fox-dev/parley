---
spec_id: "04"
spec_name: "authentication_and_login"
title: "Authenticate callers and sign users in"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "55413fe86ebee9bb8629f3e045c2804cbd11c957715283bac2f61c3db830c82d"
schema_version: 3
source: "docs/prd/03_platform.md"
---

# Authenticate callers and sign users in

## Intent

Every request to the Parley REST API is made by a known caller or is refused. This package gives the hub its three credential types (the admin token, the API key of a person and the personal access token of automation), decides for every request who is calling and whether the credential still stands, and gives a person the one way to obtain an API key: signing in through an identity provider with `parley login`, which also creates the user and the user's personal organization the first time.

## Goals

- A request to a non-public route under `/api/v1` passes authentication only with a live admin token, API key or PAT presented as `Authorization: Bearer <credential>`; the routes behind it learn the credential's type, its id, its user and, for a PAT, its permissions, paths and workspace binding.
- Every authentication failure is one of five fixed answers: `401` with the message `authentication required`, `invalid credentials`, `credential revoked` or `credential expired`, or `403` with `user is blocked`; revocation, expiry and blocking are reported only after the secret matched.
- The hub stores a hash of each secret and never the secret.
- `GET /api/v1/auth/providers` lists the configured identity providers with an authorize URL, without a credential and without exposing a client secret.
- `POST /api/v1/auth/callback` exchanges an OAuth code for the user and a new API key, creates the user and the personal org at the first login, and revokes the user's older API keys at every login.
- `parley login` signs a developer in through the browser, or stores a credential given with `--api-key`, and saves the hub URL, the credential and the user id to the client configuration file with mode 0600.

## Non-goals

- The rate limit per credential and per client address (PLAT-FR-025, which lies inside this package's range of ids): package 02 http_api_conventions. This package only reports the credential id that the limiter counts by.
- The error envelope, the order of checks, the list shape, the body rules, `Cache-Control: no-store` as the default and the rule that `Idempotency-Key` is ignored on `POST /auth/callback`: package 02 http_api_conventions.
- Loading `config.toml` (`[[oauth.providers]]`, `[server].external_url`), opening the store, creating the admin token and storing its hash and the bootstrap `admin_email`, and scrubbing of credentials from logs: package 03 server_lifecycle_and_configuration.
- The routes `/user`, `/users…` and `/orgs…`, user selectors, the admin check, blocking and unblocking as operations, user and org deletion, and `POST /users` (which reuses the personal-org rule built here): package 05 users_and_organizations.
- Creating, listing, changing and revoking PATs, refreshing and revoking API keys, the scope registry and the scope check, and `paths` validation: package 06 api_keys_and_access_tokens. This package stores the PAT record and authenticates it; it has no route that creates one.
- The workspace access rule: package 07 workspace_access_rule. The binding guard for bound PATs: package 08 workspace_bound_tokens.
- The client configuration file as every command reads it, the global flags, the general rules for output, error printing and exit codes, and the request timeout (PLAT-FR-090 to PLAT-FR-095): package 09 cli_foundation. This package writes the file at login and gives `parley login` the exit codes its own entry states.
- Authentication of git requests, where the credential is the HTTP Basic password: package 17 git_smart_http, which calls the credential check built here.
- A password login, and any other route that creates an API key: not part of the product (PLAT-FR-033).

## Background

The repository holds no code yet; every file named here is new. Package 01 hub_skeleton delivers `parleyd` and `parley` with `internal/server/` and `internal/cli/`. Package 02 http_api_conventions delivers the request pipeline with a position for authentication at step 2 of the check order, the error envelope and the list helper. Package 03 server_lifecycle_and_configuration delivers `internal/config/`, the embedded store in `internal/store/` and the admin-token bootstrap.

The source is `docs/prd/03_platform.md`: PLAT-FR-020 to PLAT-FR-033 (without PLAT-FR-025), §3.3.2 (credential types), the parts of §3.3.3 and §3.3.4 that a login creates and authentication reads, PLAT-FR-035 as far as a login needs it, the routes `GET /auth/providers` and `POST /auth/callback` of §3.5, `parley login` of §3.6, §3.3.7 as far as login writes the file, and the OAuth entries of §3.10.

The code of this package lives in `internal/identity/` (records, credentials, authentication, OAuth client, sign-in, the two routes), `internal/cli/login.go` and `internal/cli/config.go` (the command and the file it saves), `internal/hub/identity.go` (wiring on the composition of package 03) and `test/e2e/login_test.go`.

Because packages 05 and 06 come after this one, this package creates the records a login writes and authentication reads: users, orgs, memberships, API keys and PATs, with the fields of §3.3.3. Packages 05 and 06 add the routes over them.

## Requirements

### Credential types and presentation (§3.3.2, PLAT-FR-020)

| Type | Wire format | Identity |
|---|---|---|
| Admin token | `parley_admin_<64 lowercase hex>` | none (no user) |
| API key | `parley_<key_id>_<secret>`; `key_id` 8 and `secret` 32 characters from `[0-9A-Za-z]` | a user |
| PAT | `parley_pat_<token_id>_<secret>`; `token_id` 8 and `secret` 32 characters from `[a-z0-9]` | a user |

The hub stores only a hash of each secret. A secret is shown once, in the response that creates it. REST calls present the credential as `Authorization: Bearer <credential>`; the scheme name `Bearer` is matched without regard to case.

Every route under `/api/v1` needs a credential, except `GET /auth/providers` and `POST /auth/callback`. A public route answers whether or not a credential is sent, and a bad credential sent to it is not refused.

When a request passes, the routes and checks behind authentication learn: the credential type (`admin`, `api_key` or `pat`), the credential id (the fixed id `admin` for the admin token, otherwise `key_id` or `token_id`), the user with role and status (none for the admin token) and, for a PAT, its `permissions`, `paths` and `workspace`. Package 02's rate limit counts by the credential id.

### Authentication answers (PLAT-FR-020, PLAT-FR-021, §3.3.4)

Checks run in this order, and the first that fails decides the answer:

| Check | Failure |
|---|---|
| header present, scheme `Bearer`, token non-empty and of a known format | `401 unauthenticated`, message `authentication required` |
| credential id exists and the secret matches | `401 unauthenticated`, message `invalid credentials` |
| not revoked | `401 unauthenticated`, message `credential revoked` |
| not expired | `401 unauthenticated`, message `credential expired` |
| user not blocked | `403 forbidden`, message `user is blocked` |

Revocation and expiry are reported only after the secret matched, so a caller who knows only an id cannot probe a credential's state: a revoked, expired or blocked user's credential presented with a wrong secret gets `invalid credentials`. Secrets are compared in constant time.

Blocking revokes nothing. While a user is blocked, every credential of that user fails with `403`; once the user is `active` again, every credential that is neither revoked nor expired authenticates again. A credential with `expires_at` null never expires.

The admin token has no user. A route that needs a user (the routes under `/user` and the bound-token mint, which later packages register) answers the admin token `403 forbidden`.

### Identity providers (PLAT-FR-030, §3.5, §3.9)

The supported providers are `github` and `google`, each configured by an `[[oauth.providers]]` entry with `name`, `client_id`, `client_secret` and optional `authorize_url`, `token_url` and `userinfo_url` overrides (for example for GitHub Enterprise).

`GET /auth/providers` is public. It answers `200` with a list of `{"name","authorize_url"}`, ordered by `name`, with `Cache-Control: public, max-age=300`. `authorize_url` carries the client id and the scopes the hub needs: `user:email` for GitHub (`https://github.com/login/oauth/authorize?client_id=…&scope=user%3Aemail`), `openid email profile` for Google. The caller adds `redirect_uri` and `state`. Client secrets are never exposed. With no provider configured the list is empty.

### The login callback (PLAT-FR-031, §3.5)

`POST /auth/callback` is public. Its body:

| Field | Type | Required | Default | Validation |
|---|---|---|---|---|
| `provider` | string | yes | | a configured provider |
| `code` | string | yes | | non-empty |
| `redirect_uri` | string | yes | | see below |
| `expires` | int | no | 90 | 0, 30, 60 or 90 (days) |

`redirect_uri` is allowed when it is `http` on `localhost` or `127.0.0.1` (any port and path), or when its scheme, host and port equal those of `[server].external_url`.

The hub validates the body, validates `redirect_uri`, exchanges the code with the provider, reads the user's identity and then, atomically, upserts the user and issues an API key. Calls to the provider time out after 15 seconds. It answers `200`:

```json
{"user":{"id":"uuid","username":"alice","email":"a@x","full_name":"","role":"user","status":"active",
  "provider":"github","provider_id":"123","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z"},
 "api_key":{"key":"parley_<key_id>_<secret>","key_id":"<key_id>","expires_at":"2026-04-01T00:00:00Z"}}
```

| Status | `error_type` | When |
|---|---|---|
| 400 | `invalid_request` | missing field, bad `expires`, unknown provider, `redirect_uri` not allowed |
| 401 | `unauthenticated` | the provider rejected the code |
| 403 | `forbidden` | the user is blocked |
| 409 | `conflict` | uniqueness conflict |
| 502 | `upstream_error` | the provider failed, timed out, or returned no user id or verified e-mail |

On every failure nothing is changed: no user, org, membership or key is created and no key is revoked.

### The identity the provider gives (PLAT-FR-031)

The identity MUST include a stable provider user id and a verified e-mail. GitHub's primary verified e-mail is used when the profile hides the address. A missing id or e-mail is `502 upstream_error`. The username is the provider's login or display name, or `<provider>-<provider_id>` when the provider gives none.

### Users at login (PLAT-FR-032, PLAT-FR-035, §3.10)

**New user** (no user with this `provider` and `provider_id`): created `active`. If the username is taken, the hub tries `<name>-2` … `<name>-10`. The role is `admin` when the e-mail equals the bootstrap `admin_email` (ignoring case) and no admin user exists yet; otherwise `user`.

The new user gets a personal org in the same transaction: `name` is the username, `owner_id` is the user, and the user is its first member. The slug is derived from the username:

1. lowercase; replace every character outside `[a-z0-9_-]` with `-`; collapse runs of `-`; trim leading and trailing `-` and `_`;
2. if fewer than 2 characters remain, use `u-<first 8 characters of the user id>`;
3. if it starts with a digit, prefix `u-`;
4. if the slug is taken, try `<slug>-1` … `<slug>-10`.

Examples: `Alice.Smith` → `alice-smith`; `42bob` → `u-42bob`. If no free slug is found, or an org already has the user's name, the login fails with `409 conflict` and nothing is created.

**Existing user** (matched by `provider` and `provider_id`): a blocked user is refused with `403` and nothing changes. Otherwise `email` and `username` are refreshed from the provider. If the new username is taken, the old one is kept silently.

**Both:** every live API key of the user is revoked, including one in use on another machine, and a new key is issued with the requested lifetime. PATs are not touched. A uniqueness conflict (for example an e-mail that belongs to another account) is `409 conflict` and changes nothing.

Constraint (PLAT-FR-033): only the OAuth callback creates API keys. There is no password login.

### `parley login` (§3.6, §3.3.7, PLAT-FR-090)

Flags: `--provider <name>` (default `github`), `--expires <days>` (default `90`; 0, 30, 60 or 90), `--api-key <key | ->` (skip the browser and store this credential; `-` reads it from stdin). The hub URL is the first non-empty value of `--endpoint-url`, `PARLEY_ENDPOINT_URL` and the file key `endpoint_url`.

**Browser flow.**

1. Resolve the hub URL. Fetch `GET /auth/providers` and pick the provider.
2. Listen on `127.0.0.1` on a free port. Open the provider's authorize URL in the browser with `redirect_uri=http://127.0.0.1:<port>/callback` and a random `state`. If no browser can be opened, print the URL on stderr.
3. Wait up to 120 seconds for the redirect. A `state` mismatch fails the login. The browser tab shows a short success or failure page.
4. Call `POST /auth/callback`. Save `endpoint_url`, `api_key` and `user_id` to the config file.

**Key flow (`--api-key`).** Call `GET /user` with the credential and, on success, save it. No key is created or revoked. A PAT needs `users:read`. The admin token cannot be stored this way, because it has no user: the hub refuses it.

**Output.** The `User` on stdout as one JSON document indented by 2 spaces; `Logged in as <username>` on stderr.

**Exit codes.** 0; 1 when the hub refuses, with the hub's error envelope printed on stderr as received; 2 for every client-side failure (no endpoint, connection failure, timeout, state mismatch, an invalid flag value, an unreadable or unwritable config file), with an envelope on stderr whose `code` is 0 and whose `error_type` is `usage_error` for a bad flag or argument and `client_error` otherwise.

**The config file.** `$PARLEY_CONFIG_DIR/config.toml` when `PARLEY_CONFIG_DIR` is set and non-empty, otherwise `~/.parley/config.toml`. Keys: `endpoint_url`, `api_key`, `user_id`. `parley login` creates the directory (mode 0700) and the file (mode 0600). The file is replaced atomically and keeps mode 0600. Keys the CLI does not know are preserved. An unparseable file is a client error (exit 2).

### End-to-end flows

- **First login.** A developer runs `parley login --endpoint-url <hub>`. The CLI fetches the providers from `parleyd`, listens on a loopback port and opens the browser. The developer approves at the provider, and the browser is redirected to the CLI with a code and the state. The CLI posts the code to `parleyd`, which exchanges it with the provider, reads the identity, creates the user, the personal org and an API key in one transaction and answers `200`. The CLI saves the config file with mode 0600 and prints the user. A request to a protected route with the saved key passes authentication as that user.
- **Second login.** The same developer logs in again from another machine. `parleyd` finds the user by provider identity, revokes the first key and issues a second. A request with the first key is answered `401` `credential revoked`; the second key authenticates.
- **Operator.** An operator sends a request to a protected route with the admin token that the bootstrap of package 03 wrote. `parleyd` matches it against the stored hash and passes the request as the admin caller, with no user. The same request without the header is answered `401` `authentication required`.

## Design Decisions

1. **Project decisions relied on.** Go; the store is embedded SQLite through `modernc.org/sqlite` behind `internal/store`; HTTP is `net/http`; the CLI is `github.com/spf13/cobra`; tests use `testing`, table-driven, with `net/http/httptest`.
2. **This package owns the identity records.** The tables for users, orgs, memberships, API keys and PATs, with the fields of §3.3.3, are created here through the schema migration of `internal/store`. Why: a login writes the first four and authentication reads keys and PATs, and packages 05 and 06 depend on this one. Agreed with packages 05 `users_and_organizations` and 06 `api_keys_and_access_tokens`: they add no table and no column for these records; they read and write the tables created here.
3. **A PAT can be stored without a route.** `internal/identity` has a function that generates a PAT (id, secret, hash) and stores it for a user with given permissions, paths, workspace and expiry. It takes an open store transaction, generates the id again when it already exists, and validates nothing beyond the record's shape. Agreed with package 06 `api_keys_and_access_tokens`: its minting operation is the one way a PAT is created in production; that operation validates, applies the subset rule and then calls this function for the write. No other package calls this function; this package's tests call it directly to seed PATs.
4. **Secret hash.** The hub stores the lowercase hexadecimal SHA-256 of the secret part of an API key or PAT. Why: the secrets are 32 random characters, so a fast hash is enough. The comparison uses `crypto/subtle.ConstantTimeCompare` over the two hashes. Ids and secrets are generated from `crypto/rand`.
5. **The admin token's hash belongs to package 03.** Authentication verifies an admin-format token through the verification function of `internal/identity/admin.go`, the file package 03 creates for its bootstrap code, and does not reimplement the hash. A hub that is not bootstrapped answers an admin-format token `401` `invalid credentials`.
6. **A credential is recognised by its exact format**: `parley_admin_` plus 64 characters of `[0-9a-f]`; `parley_pat_` plus 8 and 32 characters of `[a-z0-9]` joined by `_`; `parley_` plus 8 and 32 characters of `[0-9A-Za-z]` joined by `_`. The lengths make the three disjoint. Anything else is "not of a known format".
7. **The header.** The scheme and the credential are separated by one or more spaces; white space around the credential is ignored. A credential in a query parameter, a cookie or another header is not read.
8. **Expiry boundary.** A credential is expired when the current time is at or after `expires_at`.
9. **A route declares that it needs a user.** The declaration is checked right after authentication and answers the admin token `403 forbidden` with the message `this route needs a user`. The authenticator reads the declaration as a named mark of package 02's route registration. This is the one error type and message the admin token gets wherever a route needs a user: packages 05 and 06 use the declaration on every `/user…` route and state the same message, and package 08 answers the same error type and message from its handler on `POST /workspaces/:slug/tokens`, where the access check has to run first.
10. **OAuth uses the standard library only.** The code exchange is one form-encoded `POST` to the token URL with `client_id`, `client_secret`, `code`, `redirect_uri` and `grant_type=authorization_code` and the header `Accept: application/json`; the identity is read with `Authorization: Bearer <access token>`. No OAuth module is added.
11. **Provider endpoints.** GitHub: authorize `https://github.com/login/oauth/authorize`, token `https://github.com/login/oauth/access_token`, user `https://api.github.com/user`; the e-mail list is read from the user URL with `/emails` appended. Google: authorize `https://accounts.google.com/o/oauth2/v2/auth`, token `https://oauth2.googleapis.com/token`, user `https://openidconnect.googleapis.com/v1/userinfo`. The PRD says only "the provider's public endpoints".
12. **Google's `authorize_url` also carries `response_type=code`**, which Google requires; GitHub's carries exactly `client_id` and `scope`, as in the PRD's example.
13. **Rejected or failed.** The provider "rejected the code" (`401`) when the token endpoint answers a JSON object with an `error` member, with any status below 500. Every other outcome without an access token — a network error, a timeout, a status of 500 or above, a body that is not a JSON object — and every failure of the identity read is "the provider failed" (`502`).
14. **Identity fields.** GitHub: `id` (as a decimal string) is the provider user id, `login` the username, `name` the full name; the profile's `email` is used when non-empty, otherwise the entry of the e-mail list with `primary` and `verified` true. Google: `sub`, `name` as the username and the full name, `email` only when `email_verified` is true. A username is used as given after trimming white space; an empty one becomes `<provider>-<provider_id>`.
15. **`full_name`** of a new user is the provider's display name, or empty; it is not refreshed at later logins, because the user can change it with `PATCH /user`.
16. **Username collisions for a new user** that exhaust `<name>-2` … `<name>-10` are a `409 conflict`.
17. **"No admin user exists yet"** counts users with role `admin` in any status.
18. **E-mail and username uniqueness** compare the exact stored strings. Only the comparison with `admin_email` ignores case, as the PRD states.
19. **The personal org is not changed at later logins**: a refreshed username does not rename it.
20. **Key lifetime.** `expires_at` is the issue time plus `expires` × 24 hours; `expires` 0 gives `expires_at` null.
21. **Error messages of the callback are fixed texts** and never contain the code, a client secret, an access token or the provider's response body.
22. **`parley login` reads and writes the config file itself**, in `internal/cli/config.go`, and prints its own errors and exit codes with an error type kept in `internal/cli/login.go`. Why: package 09 `cli_foundation`, which generalises both for every command, depends on this package. Agreed with package 09: it takes both over as the single implementation. It extends `internal/cli/config.go` into the one read and save every command uses, replaces the login command's own error type, request code and printing with its shared client, printers and exit-code mapping, and keeps the behaviour specified here; after package 09 no second copy remains.
23. **Only the flag selects the key flow.** `PARLEY_API_KEY` and the file key `api_key` are ignored by `parley login`; `--expires` and `--provider` are ignored in the key flow.
24. **A provider the hub does not list** fails the login with `client_error` and exit 2, naming the providers the hub offers.
25. **The redirect listener** serves `GET /callback` once and closes. A redirect with an `error` parameter or without `code` fails the login like a state mismatch. `state` is 32 hexadecimal characters from `crypto/rand`. `redirect_uri` and `state` are appended to `authorize_url` as query parameters.
26. **Opening the browser** runs `open` on macOS, `xdg-open` on Linux and `rundll32 url.dll,FileProtocolHandler` on Windows. The opener and the 120-second wait are parameters of the command's constructor, so tests replace them.
27. **`endpoint_url` is saved without trailing slashes.** `user_id` is the `id` of the returned `User`.
28. **An unparseable config file fails `parley login` before any request**, so that a key is not issued and then lost. A file that cannot be written after a successful callback is exit 2 with a message that says the key was issued and not saved.
29. **TOML** is read and written with `github.com/BurntSushi/toml`, decoding into a map so unknown keys survive. Comments in an existing file are not preserved.
30. **Profile: full**, with three execution paths.
31. **The personal-organization rule (PLAT-FR-035) is owned here.** Login needs it, so this package specifies and builds it: one exported function of `internal/identity` that takes an open transaction, the user's fields (username, e-mail, full name, provider, provider id) and the role, and creates the user, the personal org with its derived slug and the membership, or returns a conflict error. The sign-in chooses the username (the `-2` to `-10` suffixes) and the role before it calls the function. Package 05 `users_and_organizations` calls the same function from `POST /users` with role `user` and does not restate the slug rule.

## Dependencies

- Package 02 http_api_conventions (draft): the request pipeline with the authentication position, public routes, the error envelope, the list helper and the per-route `Cache-Control`.
- Package 03 server_lifecycle_and_configuration (draft): `internal/config` with `[[oauth.providers]]` and `[server].external_url`, `internal/store` with transactions and schema migration, the stored admin-token hash with its verification function, and the bootstrap `admin_email`.
- Identity providers GitHub and Google, over HTTPS, at login only. When a provider is unreachable, login fails with `502`; authentication of existing credentials does not depend on it.

## Verified External API

`github.com/BurntSushi/toml` v1.6.0, read in the module cache (`decode.go`, `encode.go`):

- `func Unmarshal(data []byte, v any) error`
- `func Marshal(v any) ([]byte, error)`

`github.com/spf13/cobra` v1.10.2, **unverified** in this run (package 01 read `AddCommand` in the module cache):

- `func (c *Command) AddCommand(cmds ...*Command)`
- `func (c *Command) Flags() *pflag.FlagSet`

## Glossary

- **Hub**: The `parleyd` server.
- **Scope / permission**: A string carried by a PAT that grants read or write access to an area.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Admin token**: The operator's credential, written at first boot. It has no user.
- **API key**: The credential of a person at a terminal, issued only by a login.
- **PAT**: A personal access token, the credential of automation; the only one that carries scopes, a workspace binding or path limits.
- **Personal org**: The organization created with a new user, named after the username, with the user as owner and first member.
- **Live**: Of an API key or PAT: neither revoked nor expired.
