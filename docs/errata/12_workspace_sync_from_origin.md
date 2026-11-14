# Errata: 12_workspace_sync_from_origin

Where the implementation of package 12
(`.specs/12_workspace_sync_from_origin`) differs from what the package
says, or decides what it leaves open.

## The sync operation and its collaborators

- The sync is the method `(*workspace.Service).Sync(ctx, slug, opts)`,
  taking the slug and a `workspace.SyncOptions` value, so that a test runs it
  without an HTTP request; it returns a `workspace.SyncResult`. The package
  writes it as `Sync(ws)`.
- There is no stub credential resolver. The resolver stays package 10's
  `workspace.ResolveOriginCredential` over the workspace's secrets
  (`Deps.Secrets`), called at the start of every sync; tests change the
  stored secrets instead of stubbing it.
- The fetcher is the interface `workspace.Fetcher` (`FetchOrigin`), the
  field `Deps.Fetcher`, which the hub sets to its `gitrepo.Git` in
  `internal/hub/workspace.go` (nil is `gitrepo.Git{}`); tests wrap it
  through `(*hub.Composition).AdjustWorkspace`.
- `FetchOrigin` is a method of `gitrepo.Git`, not of `gitrepo.Repo`,
  because it talks to origin with a credential, which no `Repo` method
  does: `(gitrepo.Git).FetchOrigin(ctx, dir, url, cred, deadline)`, where
  the package writes `FetchOrigin(hub, originPath, credential)`. A deadline
  that passes is an error matching `gitrepo.ErrDeadline`.

## How origin's tags reach the copy

Git hands over the object of a tag only for a ref it writes, over every
transport (the file-based HTTP transport cannot fetch an object by its id).
So the one fetch also writes origin's tags into a namespace of its own,
`refs/parley/fetch/<16 random hex digits>/`, in the same atomic transaction
as the origin heads, reads them from there and deletes the namespace before
it returns, after a failure too. The copy's `refs/tags/` is never written by
the fetch; the sync then creates the tags the copy lacks from the values the
fetch returned. The tag objects stay in the copy's object store until a ref
names them or git prunes them as unreachable.

`(*gitrepo.Repo).CompareAndSwapRef` takes `""` as the expected value of a
ref that must not exist, and also answers `gitrepo.ErrRefMoved`, changing
nothing, when another writer holds the ref's lock, since that writer is
moving it at that moment.

## The sync route

- There is no `internal/workspace/routes.go`: the workspace routes are the
  list `(*workspace.Service).Routes()` in `internal/workspace/service.go`,
  which `internal/hub/workspace.go` registers. The sync route is defined in
  `internal/workspace/sync_handler.go` and added to that list, so the hub
  needed no change of its own for it; the fetcher was already wired there.
- The router's patterns name parameters with a colon: the route is
  declared as `POST /workspaces/:slug/sync` under `/api/v1`, where the
  package writes `/api/v1/workspaces/{slug}/sync`.
- `reset_to_origin` given more than once is `400 invalid_request` too,
  like the other boolean query parameters of the workspace routes, rather
  than one of its values chosen.
- The stub sync operation recording its reset argument is not used: the
  route's tests send the three accepted queries against a workspace with a
  diverged branch and read the reset from the branch's result (`diverged`,
  `diverged`, `reset`).
