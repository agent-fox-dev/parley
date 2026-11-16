# Errata: 31_carry_patch_setup_and_patches

Where the implementation of package 31
(`.specs/31_carry_patch_setup_and_patches`) differs from what the package
says, or decides what it leaves open.

## The hub part, not cmd/parleyd

The package registers the post-clone step and the routes from
`cmd/parleyd/main.go`. `cmd/parleyd` is a thin `main` that is never edited
([ADR 01](../adr/01-compose-the-hub-in-one-package.md)), so they are
registered by the part `composeCarryPatch` in `internal/hub/carrypatch.go`,
the last entry of `parts`, which also adds the schema step, registers the
patch-branch lookup of the push route and the remover of a workspace's
patches ([ADR 07](../adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)),
and binds the component in its start hook `carry_patch`.

## Operations are methods of the component

The package writes `FetchUpstream(ctx, workspace)`, `UpstreamBase(workspace)`,
`EnsureSetup(ctx, workspace)` and `ResolveBranch(ctx, workspace, branch,
skip)` as functions. They need the store, the secrets, the hub's git and the
place of each copy, so they are methods of `*carrypatch.Component`, bound
once with `Bind(carrypatch.Deps)`, as the other features' components are;
`UpstreamBase` takes a context too. `IntegrationBranch(w)` and
`ResolveUpstreamCredential(ctx, secrets, slug)` stay functions; the latter
takes the secret reader as `workspace.ResolveOriginCredential` does.

## The upstream fetch asks for the default branch first

The package asks the upstream for its default branch after the fetch.
`FetchUpstream` lists the upstream (`ls-remote`) first, then fetches, then
writes `parley.upstreamDefaultBranch`, so that a failure of either network
operation leaves the upstream refs and the recorded default branch as they
were. Both, and the write of the record after them, share one 10-minute
deadline set when the fetch starts: each runs through the runner with what
the earlier ones left of it, so that the whole upstream fetch, the listing
included, never takes longer than 10 minutes.

## The set-up's error tells a missing base from a failed fetch

When the upstream fetch succeeded but no upstream base resolves, the set-up
still writes the integration branch at `head_sha`, and its error names an
`upstream base` failure that matches `ErrNoUpstreamBase`, not
`ErrUpstreamFetch`, so that the rebuild and the sync can tell a failed
fetch from an upstream without a usable default branch. The clone logs it
like any other failed step. The write of `deploy` into the record is one
conditional `UPDATE … WHERE integration_branch IS NULL` that leaves
`updated_at`, like the clone's own writes.

## The add reads the record before the guard

The package validates the body before the guard. Refusing the integration
branch as `branch_name` needs the workspace's `integration_branch`, so the
add reads the record (`workspace.BySlug`) before the guard; no patch is
looked up and no branch resolved before the body is valid, and an invalid
body is `400 invalid_request` on any workspace. The guard takes only the
request: `carrypatch.Route` attaches the route's kind (read or write), so a
handler cannot guard with another kind than its declared need.

## What branch resolution answers beyond the package

- A ref in the way of the branch's name, such as `refs/heads/feat` when
  `feat/x` is resolved, in the copy's branches or origin-tracking refs, is
  `409 conflict` (`the branch "feat/x" cannot be created in the hub's copy:
  refs/heads/feat is in the way of its name`); nothing is written and, on
  the origin path, origin is not asked.
- A workspace without `git_url` on the origin path is `502
  origin_fetch_failed`.
- Every deleted patch of the branch is removed by the add, not one, so the
  list with `include_deleted=true` shows the branch once.

## The runner removes the user name too

`RunGit` cleans a failure's text of every value of the credential, the user
name included, besides URL user information. A very short user name would
also be cut out of ordinary words of a message.

## Checks

- The deadline checks of the upstream fetch and the single-branch fetch use
  a 1-second deadline instead of 200 ms, with a return bound of the deadline
  plus 2 seconds: under the full suite's load git did not reach the
  never-answering server within 200 ms.
- The rotation check of the upstream credential changes the secret in the
  fake reader between two calls, rather than writing it through the secret
  component; the resolver reads only through the reader.
- The status the set-up observes is recorded by wrapping `Deps.Secrets`
  through `AdjustCarryPatch`, so the registered step stays the hub's own.
- The branch-resolution checks bind the component to a real bare copy, a
  counting test origin, a fake secret reader and the real settings registry,
  without a store or a user; the add's tests cover those.
