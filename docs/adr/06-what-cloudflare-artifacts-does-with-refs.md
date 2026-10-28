# 6. What Cloudflare Artifacts does with refs

## Status

Accepted. Established by experiment on 2026-10-27 against a real namespace,
not inferred from documentation.

## Context

Three things Parley's git server, branch push and carry-patch packages all
depend on are not stated in the Artifacts documentation:

1. whether the store accepts a ref outside `refs/heads/` and `refs/tags/`;
2. whether it accepts a non-fast-forward update;
3. whether it accepts a ref deletion.

Each answer changes a design. Without arbitrary namespaces there is nowhere
to keep `refs/hub/replaced/<branch>`, which the patch-divergence policy
needs. Without non-fast-forward updates a rebuild cannot move an
integration branch. Without deletion a retired branch can only be
tombstoned. Guessing and being wrong would invalidate three packages, so
this was settled before any of them was written.

## The experiment

A scratch repository was created in the `parley-dev` namespace
(`wrangler artifacts repos create`), pushed to with an ordinary git client
over the repo-scoped token, and deleted afterwards. The store identifies
itself as `agent=gitty/1.0`.

| Attempt                                                   | Result                                                             |
| --------------------------------------------------------- | ------------------------------------------------------------------ |
| `git push origin main` into an empty repository           | accepted                                                           |
| `git push origin main:refs/hub/replaced/probe`            | accepted; the ref appeared in `ls-remote`                          |
| `git push origin main` after rewinding, without `--force` | refused, as git refuses any non-fast-forward                       |
| `git push --force origin main`                            | accepted; the remote ref moved backwards onto the divergent commit |
| `git push origin --delete refs/heads/doomed`              | accepted; the ref disappeared                                      |
| `git push origin --delete refs/hub/replaced/probe`        | accepted                                                           |

The store's own advertisements agree:

- `info/refs?service=git-receive-pack` advertises `report-status`,
  `report-status-v2`, **`delete-refs`**, `atomic`, `ofs-delta`,
  `side-band-64k`.
- `info/refs?service=git-upload-pack` with `Git-Protocol: version=2`
  advertises `version 2`, `ls-refs=unborn`,
  `fetch=shallow filter sideband-all`, `object-format=sha1`. Protocol v1 is
  served to a client that does not ask for v2.

## Decision

Parley declares all three capabilities **present** for the Cloudflare
store, and the later packages are designed on that:

| Capability                                  | Present         | What may rely on it                                                |
| ------------------------------------------- | --------------- | ------------------------------------------------------------------ |
| refs outside `refs/heads/` and `refs/tags/` | yes             | `refs/hub/replaced/<branch>` for a replaced patch branch           |
| non-fast-forward update                     | yes, with force | moving an integration branch after a rebuild; a forced branch push |
| ref deletion                                | yes             | retiring a branch, and the git server relaying a client's delete   |

Two further facts are recorded because the implementation rests on them:

- **The binding has no ref listing and no write API.** `env.ARTIFACTS` offers
  `create`, `get`, `import`, `list` and `delete` at the namespace, and a
  repository handle offers `createToken`, `info`, `readBlob`, `readTree`,
  `readCommit`, `readFile`, `log` and `fork`. Every ref read and every write
  therefore goes over the git remote with a minted repo-scoped token, which
  is why the platform interface speaks refs and packs rather than mirroring
  the SDK (see [ADR 2](02-store-repositories-as-refs-and-packs-over-our-own-git-client.md)).
- **The store speaks protocol v2 with `filter`**, so a copy can ask for
  exactly what it needs rather than everything.

The declaration is held against the store by
`src/repo/capabilities.integration.test.ts`, which reads both
advertisements and fails if they stop agreeing with it. It skips when no
namespace is configured, so the fast gate does not need one.

## Consequences

- `07_git_server`, `08_sync_and_branch_push` and `09_carry_patch` need no
  workaround for any of the three. No tombstones, no side table for refs
  outside the two namespaces, no rebuild-by-recreation.
- An operation that needs a capability still asks for it through
  `requireCapability`, so a store that lacks one — the non-Cloudflare
  implementation, or a future backend — fails with the capability named
  rather than silently doing something else.
- The answers are true of `gitty/1.0` in open beta. The integration test is
  what will notice if a later version withdraws one.
