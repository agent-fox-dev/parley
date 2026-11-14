# Errata: 13_workspace_push_to_origin

Where the implementation of package 13
(`.specs/13_workspace_push_to_origin`) differs from what the package
says, or decides what it leaves open.

## The push operation and its collaborators

- The push operation is `(gitrepo.Git).Push(ctx, gitrepo.PushRequest)`, a
  method of `gitrepo.Git` like `FetchOrigin`, because it talks to origin
  with a credential, which no `gitrepo.Repo` method does. The copy, the
  origin URL, the credential, the commit id, the branch name and whether
  the push is forced are members of `PushRequest` rather than separate
  arguments, so that a later choice of the push is one more member and no
  caller changes. The lease of a forced push is the member `Lease`, the
  commit id origin's branch must still have or `""` for a branch origin
  must not have, read only with `Force`. It returns a `gitrepo.PushResult`
  (`pushed`, `forced`, `up_to_date`) or an error; a non-fast-forward
  refusal matches `gitrepo.ErrNonFastForward`, a stale lease
  `gitrepo.ErrOriginMoved`. Every other failure is a `*gitrepo.PushError`
  whose `Class` is one of `ErrOriginRejected`, `ErrUnauthenticated`,
  `ErrForbidden`, `ErrRepositoryNotFound`, `ErrUnreachable` and
  `ErrPushFailed` ("other"), matched with `errors.Is`, and whose `Reason`
  is what origin or the transport said.
- The push deadline is not a parameter of the push operation but of the
  handler: `Deps.PushDeadline` (0 is the fixed 2 minutes in `Bind`). The
  handler runs the push under a context with that deadline; the push
  operation kills git with every process it started when the context ends
  and returns the context's error, which the handler answers `504
  origin_timeout`.
- The handler is not built from its collaborators as parameters. As for
  the sync, they are fields of `workspace.Deps`, which the hub sets in
  `internal/hub/workspace.go` and tests change through
  `(*hub.Composition).AdjustWorkspace`: the push operation is the
  interface `workspace.Pusher`, the field `Deps.Pusher` (the hub's
  `gitrepo.Git`; nil is `gitrepo.Git{}`); the credential resolver is
  `workspace.ResolveOriginCredential` over `Deps.Secrets`; the store is
  `Deps.Store`.

## The test origin

The push mode of the one test origin is the constructor
`gitrepotest.SmartOrigin(t, opts...)`: `NewOrigin` with the smart transport
and `http.receivepack` enabled in the bare repository, since
`git http-backend` refuses a push from a client it knows no user of
otherwise. It keeps the options and switches of `NewOrigin`
(`WithBasicAuth`, `Requests`, `Head`, `Block`, `Refuse`, `Close`) and adds
`Accept(user, password)`, which changes the credential the origin takes
while it serves, `Reject(message)`, which installs a `pre-receive` hook
that refuses every update with that message, and `ServeUntrusted(bool)`,
which makes it serve a self-signed certificate its `CAFile` does not vouch
for. Every `httptest` TLS server serves the same certificate, so a second
origin would be trusted by a hub that trusts the first; the untrusted
certificate is therefore a switch of the one origin. The stall is
`Block()`, and "listener closed" is a second origin that was closed, which
the workspace's `git_url` is pointed at for that case.
