# Errata: 55_acceptance_suite

Where the acceptance suite (`test/acceptance`) differs from what package 55
(`.specs/55_acceptance_suite`) says. The suite adds no product code: where
the package and the hub disagree, the scenario follows the hub, and a
difference that leaves a criterion unmet is a tracked issue.

## No test reads the PRD

The package's convention scenario compares its route table with the routes
parsed from `docs/prd/appendix_a_routes.md`. The project's steering forbids
tests that check what a PRD says, so no test reads `docs/prd/`. The table in
`test/acceptance/routes_test.go` was generated once from that index (220
routes under `/api/v1`, every one registered on the hub with the same method
and path), says so in its file comment, and is generated again when the
index changes. Each row is proved against the running hub instead: a request
without a credential is answered 401 (the callback's malformed body 400) and
a method the path lacks is answered 405.

## The configuration directory holds `config.toml` only

A hub that has its admin token refuses to start while `admin_token` is still
in the configuration directory (`internal/identity/bootstrap.go`). The
harness reads the token that `parleyd --admin-email` wrote and deletes the
file before it starts the hub, so the one-binary scenario asserts the
configuration directory holds `config.toml` alone.

## What every hub of the suite is configured with

Besides the server address, `external_url` and the fake identity provider,
every hub gets `[limits] requests_per_minute = 0` (the default 600 a minute
throttles the suite, and git's unauthenticated first requests count against
the loopback address) and `[changes] mergeable_debounce = "0s"` (otherwise
mergeability is reported up to 5 s late). A scenario adds only what it
needs: `[land] max_attempts = 20` for twenty concurrent lands,
`[awareness] max_claims_per_workspace = 1000000` for the writers killed
under load and those writing while a backup runs (they pass the default
1,000 within seconds), `[webhooks] allow_insecure = true` for the loopback
receiver, `[events] retention = "1h"` for the expired cursor (the shortest
retention the hub takes), the variable `SSL_CERT_FILE` naming the fake
forge's certificate for the hubs that reach it, and, against a gateway, a
`[sandbox]` section with the gateway's URL, a token file and the profiles a
scenario names.

## Rows left out of the paging and idempotency sweeps

The paging and idempotency scenarios sweep every list and create route of
the table but eight, each marked in it with its reason; all eight stay in
the envelope scenario's 401 and 405 sweep:

- `GET /api/v1/auth/providers`: the configuration names at most two
  providers, so a list of three cannot exist.
- The six sandbox lists (`GET /api/v1/sandbox-profiles`,
  `/api/v1/sandboxes`, `/api/v1/workspaces/:slug/sandboxes`, a sandbox's
  `logs` and `proposals`, and `/api/v1/workspaces/:slug/sandbox-rules`)
  and the sandbox create (`POST /api/v1/workspaces/:slug/sandboxes`): the
  suite's hubs have no OpenShell gateway.

The five forge lists and five forge creates are swept against the
harness's fake GitHub API, which pages by `per_page` and `page` with a
`Link` header as GitHub does; the workspace writes to it with a `GIT_PAT`
secret.

## Names the package has wrong

The scenarios use the hub's names where the package's differ and the intent
does not: `workspace create --slug`, the version member `number`, the land
attempt trigger `auto_land`, `…/scm/refs/<branch>`, an API commit's
`message` and `op: "put"`, the scope `git:write` (there is no `scm:*`),
`PATCH` of a ref with `sha` and `expected_sha` (there is no `force`), a
policy rule's requirements under `require`, the rebuild status `completed`,
an interdiff of a pure rebase listing every file with `changed: false`, and
`oldest_seq` inside `error`. Every change push carries `-o parley.intent`,
since `changes.require_intent` is on by default.

## Refusals that come from another rule

- An organization member with read-only access is refused before the
  protected-branch rule: over git with `org members have read-only access to
  this workspace; create a task fork to contribute`, over the API with
  403 `forbidden`. The member's rows assert that refusal and an unmoved
  branch; the bound and path-scoped tokens get `protected_branch` as the
  package says.
- A fork's default branch is protected as well as tracking, and the
  protected-branch rule comes first, so a push to the fork's `main` with its
  token is refused with `main is protected; push another branch and open a
  change`. The tracking-branch text is asserted on a tracking branch that is
  not protected, and on `main` pushed with the owner's API key.

## Who may report a check

Only the workspace owner or the admin token may report checks, so the
package's separate "ci" user with write access cannot exist: "ci" is a
second PAT of the owner holding `checks:write` (and `audit:read`,
`changes:read` where it reads the stream). A read-only member may not start
a session on the workspace either; the awareness scenario starts the
member's session on the member's own fork, which the parent then accepts.

## Where the events are, and in what order

A fork's own events (its creation, pushes to it, its expiry) are filed under
the fork's slug, so the agent journey reads the event log without a
workspace filter and the expiry scenario by type. A push maps itself to
changes before its `parley.git.push` is written, so `parley.change.open`
comes before the push event that opened it. Land attempts are listed newest
first, and those within one second in no fixed order, so the race journey
compares each change's attempts as a set.

## The event log's last page

The event log gives every full page a `next_cursor`, as package 52 says, so
a walk with `limit=1` ends on an empty page with `next_cursor: null` rather
than on a last full page with a null cursor. The route table marks that one
row, and the paging scenario allows that end page there only.

## Gaps tracked as issues

- The sandbox scenarios were not run against an OpenShell gateway, and with
  the pinned SDK (create in a working directory, the main command, logs and
  policy validation answer `unsupported`) they are expected to fail against
  one. Two optional variables help a remote gateway reach the hub:
  `PARLEY_ACCEPTANCE_SANDBOX_HUB_HOST` and
  `PARLEY_ACCEPTANCE_OPENSHELL_PROVIDER_PROFILE`; only the gateway URL gates,
  so a gateway set up halfway fails the scenarios instead of skipping them,
  and without `PARLEY_ACCEPTANCE_SANDBOX_IMAGE` they fail at once.
- The 1 GiB repository and reference-host scenarios were not run here.
