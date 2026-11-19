# Errata: 45_sandbox_creation_and_credentials

Where the implementation of package 45
(`.specs/45_sandbox_creation_and_credentials`) differs from what the package
says, or decides what it leaves open.

## The runtime sets the placeholders of the providers

The package passes the sandbox an environment that holds, for each attached
credential, the variable its provider profile declares with the credential's
placeholder, and `PARLEY_API_KEY` with the placeholder of the hub
credential. The runtime client has no operation that tells the hub a
placeholder: OpenShell itself puts the placeholder of each provider attached
to a sandbox into the variables the provider's profile declares (the
`parley-hub` profile declares `PARLEY_API_KEY`), see
[the errata of package 44](44_sandbox_runtime_and_profiles.md).

So the environment the hub passes leaves out every variable an attached
provider's profile declares, read with `GetProviderProfile`, and
`PARLEY_API_KEY` when the sandbox has a hub credential, and drops any entry
of the creator's `env`, the profile's `env` or an exposed secret of that
name. The placeholder the runtime puts there is the value, so placeholders
still win over the earlier entries, and the real value of a credential
never enters the environment. Code: `internal/sandbox/environment.go`; test:
`TestProvision_Environment`.

## A workspace whose clone runs is busy, not "not ready"

The package answers `409 workspace_not_ready` for a workspace whose
`status` is anything but `ready`. The create applies the workspaces' guard,
`workspace.RequireReady`, as every other route that needs a ready workspace
does: an archived workspace is `409 workspace_archived`, one whose clone
failed `409 workspace_not_ready`, and one whose clone is `pending` or
`cloning`, or on which a reclone, archive or delete runs, `409
workspace_busy`, the answer that says the request will succeed later. A fork
of an archived parent is `409 parent_archived` (`fork.GuardUpdate`). Code:
`internal/sandbox/create.go`; test: `TestCreate_RefusesByWorkspaceState`.

## A PAT with `sandboxes:write` reads sandboxes

The package lists a PAT holding `sandboxes:write` alone as refused `403
insufficient_scope` on the two read routes. The hub's one scope check
(`identity.Satisfies`) lets `<area>:write` satisfy `<area>:read` for every
area, and the read routes declare `sandboxes:read` through package 44's
`WorkspaceRoute`, so such a PAT reads (`200`). Changing the rule for one
area would make sandbox scopes the only ones that do not imply each other.
Test: `TestRead_Authorization`.

## The secret resolution reads the chain through the fork's transaction

The fork create hands the sandbox member to the sandbox component inside
its transaction, and the sandbox component resolves the profile's secrets
along the new fork's chain before anything is created. `ResolveSecrets`
read the chain's workspace records through the store's main handle, which
does not see the fork until the transaction commits, so a profile with a
credential or an exposed secret would have failed every such create with
`500`. `(*secret.Component).ResolveSecretsIn(ctx, q, slug, names)` is the
same resolution with the workspace records of the chain read through `q`,
the caller's transaction; the memberships, the users, the write access to
the parent and the values are read as before, since the fork transaction
writes none of them. The create always resolves through its transaction;
the provisioning, after the commit, uses `ResolveSecrets`.

The session-linking rule's check (`SessionLinker.Check`) takes no
transaction: a linker that reads the workspace record will not see a fork
that is not committed yet when the sandbox member names a `session_id`.

## Collaborators are registered on the service

The package gives the session linker, the workspace rule provider, the
policy composer, the restorer and the delete and purge hooks each "a
registration function". They are methods of the `*sandbox.Service` the hub
composes (`RegisterSessionLinker`, `RegisterWorkspaceRuleProvider`,
`RegisterPolicyComposer`, `RegisterRestorer`, `RegisterDeleteHook`,
`RegisterPurgeHook`), reached through `Composition.Sandbox()`, not
package-level registries, so that each test hub has its own
([ADR 03](../adr/03-register-removers-of-owned-data-on-the-identity-service.md),
[ADR 07](../adr/07-register-the-teardown-collaborators-on-the-workspace-service.md),
[ADR 08](../adr/08-register-the-push-collaborators-on-the-ref-writer.md)).
A nil registration restores the default where there is one.

## The hub credential's token operations take a context and the instant

The package revokes the hub credential with `RevokeTokenByID(tx,
token_id)`. It is `identity.RevokeTokenByID(ctx, tx, tokenID, now)`, the
form package 06 exports ([its errata](06_api_keys_and_access_tokens.md)),
with the instant from the hub's clock.

## The scopes of the hub credential are held by name

The hub credential carries those of the six scopes the creating credential
holds. A scope counts only when the creating PAT holds it by name, because
`MintBound` holds a PAT creator to the subset rule, which compares scopes
exactly: a PAT with `git:write` alone gives a hub credential with
`git:write` alone. Whether the hub credential can clone (and so whether a
checkout is made) is judged by the scope check, so `git:write` still
clones.

## Names and labels at the runtime

A sandbox is addressed by name at the runtime: the runtime's sandbox is
named by the sandbox's id, and each provider `<id>-<credential>`, the hub
credential's `<id>-hub`, since providers are shared by the whole OpenShell
workspace; each is labelled `parley.sandbox=<id>`. A credential configured
with the name `hub` would share the hub credential's provider name.

## A started sandbox no longer shows its end

The package sets `ended_at` when a sandbox stops, completes, fails or is
deleted, and says nothing of it on a start. A move to `provisioning` or
`ready` clears it, so `ended_at` is `null` while a sandbox runs, also after
a start.

## The CLI's durations are whole units

The checks of `parley sandbox create --wait` pass `--poll-interval 10ms`
and `--timeout 50ms`. The CLI's duration form, package 09's, is a whole
number of `s`, `m`, `h` or `d`, so those values are usage errors; the tests
poll through the CLI's own test seam and use `1s`. The behaviour of the
waiting is the one the package specifies.

## OpenShell SDK v0.1.8 cannot create a sandbox in a working directory

The provisioning creates the runtime's sandbox in the working directory
`/workspace` with the profile's base document, and starts the main process
with `RunMain`. The OpenShell implementation answers each of these
`unsupported` with SDK v0.1.8 ([the errata of package
44](44_sandbox_runtime_and_profiles.md)), so against a real gateway of that
release every provisioning ends `failed` with `provision_failed`; the
behaviour is tested against the in-memory runtime.
