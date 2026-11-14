# Errata: 17_git_smart_http

Where the implementation of package 17 (`.specs/17_git_smart_http`)
differs from what the package says, or decides what it leaves open.

## The git server is composed in `internal/hub`, not in `cmd/parleyd`

The package wires the git server's collaborators "in `cmd/parleyd`". Every
part of the hub is composed in a file of its own in `internal/hub`
([ADR 01](../adr/01-compose-the-hub-in-one-package.md)), and `cmd/parleyd`
only calls `hub.Run`; so is this one. `internal/hub/gitserver.go` adds the
part `composeGitServer`, after `composeForks` in `parts`: it mounts the three
routes with `Composition.Handle` and binds the `gitserver.Component` in the
start hook `gitserver`. `Composition.GitServer()` returns the component, and
`Composition.AdjustGitServer` changes its dependencies before they are
bound, the test seam the other parts have.

## The rate limit is the server's own limiter, reached through a server hook

The package builds the git handler "from options" that include the limiter
of `internal/server`. The hub builds its HTTP server, and with it the
limiter, after the start hooks, where the component is bound; so the
component's `Limiter` is a `serverLimiter` that a server hook
(`Composition.addServerHook`, run by `install`) points at
`srv.RateLimiter()` before the first request. The key a request is counted
against comes from `server.LimitKey(r, credentialID)`, the function the
REST pipeline counts with, so that a credential has one window across git
and the REST API and a request that failed authentication counts against
the same address key.

## The checks run against a real hub

The checks of the package ask for "a hub handler from `internal/gitserver`
served through `net/http/httptest` over a real store". The tests of
`internal/gitserver` start a real hub with `hubtest.StartWith`, which serves
the real server in-process on a loopback port over a real store, and
replace the service that runs after the checks with a recorder through
`AdjustGitServer`; the git routes are thereby also reached through the
server's dispatcher, as in production.

## A workspace "without an organization" is one whose organization is gone

The package answers `404 repository not found` for "a workspace without an
organization". Every workspace record carries an `org_id` (the column is
`NOT NULL` and `access.InsertRecord` refuses an empty one), but it carries
no foreign key, so a workspace keeps the id of an organization that has
since been deleted. That is the workspace without an organization: the git
server answers it, and a record whose `org_id` is empty, with the same
`404` as an unknown slug, and its test stores a workspace whose `org_id`
names no organization.

## The two `service` values are checked at the exchange, not at its content type

The check of the package asserts that ref discovery with
`service=git-upload-pack&service=git-receive-pack` answers with the
`Content-Type` of an upload-pack advertisement. Which service serves a
request is decided by the checks, and the advertisement is written by the
service that runs after them; the test of the checks asserts that the
request reaches that service as an upload-pack request with need read. The
content type is the one the upload-pack advertisement always carries, written
by the service of ref discovery, not by the checks.

## A workspace under maintenance is answered `workspace is busy`

The package refuses a workspace "whose status is not ready" with
`409 repository not available: workspace is <status>`, and names no answer
for a workspace whose own maintenance marker is set: a reclone keeps the
status `ready` while it replaces the repository, and an archive or a delete
runs on a workspace of any status. The git server judges the workspace's own
record with `workspace.RequireReady`, the state guard of every route that
needs a repository, which reads the marker first; a set marker is answered
`409 repository not available: workspace is busy`, the message of the
package with `busy` where the status goes, the word the package uses for
the same state of a parent. A status other than ready without a marker is
named as the package says.

## A failed parent is answered as a busy one

The package refuses a fork whose parent is "in exclusive maintenance"
(a marker, pending or cloning) with
`409 repository not available: parent workspace is busy`, and an archived
parent on a push with `409 repository is read-only: parent workspace is
archived`; it names no answer for a parent whose clone failed. The parent is
judged by `(*fork.Component).CheckParentState`, the check every route that
works on a fork uses, which refuses a failed parent for both uses. The git
server answers that refusal, as every `409` of the check other than
`parent_archived`, with the busy message, so that the refusals of the git
routes stay the fixed list of the package and a fork is never served while
its parent cannot serve.

## git's own text of a refused fetch is its error output, framed by the hub

The package answers a fetch body that git cannot parse in band with "git's
own text". In stateless mode `git upload-pack` writes no `ERR` packet for a
request it cannot read (a truncated or malformed pkt-line): it writes its
reason to its standard error and exits. When git fails before any pack data
went out and wrote no error packet of its own, the hub writes one `ERR`
pkt-line with the last line of git's standard error, without its `fatal: `
or `error: ` prefix and with the repository's directory on disk replaced by
`repository` (`upload-pack failed` when git wrote nothing), so that the
client gets git's reason and never a path of the hub. The `ERR` pkt-lines
the hub writes carry no trailing newline, as git's own do.

## A version 2 want of a commit no ref reaches is refused by the hub, not by git

The package passes `uploadpack.allowReachableSHA1InWant=true` to every run
of upload-pack and expects git to refuse, in band, a want of an object no
advertised ref reaches. Tried with git 2.56.0: under protocol version 0, in
stateless mode, git refuses a want of a commit no ref reaches with
`ERR upload-pack: not our ref <id>` whatever the configuration, and the
setting only makes it advertise `allow-reachable-sha1-in-want`, without
which a version 0 client does not ask for a commit by id at all
(`Server does not allow request for unadvertised object`). Under protocol
version 2 git serves any object it holds by its id: a commit written with
`git commit-tree` that no ref points at is fetched with success, also with
`uploadpack.allowReachableSHA1InWant`, `uploadpack.allowTipSHA1InWant` and
`uploadpack.allowAnySHA1InWant` all set to `false`. No setting of git
refuses it.

So the hub refuses it itself (`internal/gitrepo/serve.go`, `checkWants`):
a version 2 `command=fetch` request is kept in a file of git's private
directory, its `want` ids are given to `git rev-list --stdin --not --all`,
and when that lists one of them, the request is answered with the `ERR`
pkt-line `upload-pack: not our ref <id>`, git's own text of version 0,
before upload-pack runs. A commit no ref reaches (one whose branch was
deleted, or a commit of the parent that a task fork can read only through
its alternates) is thereby served under neither version.

A tree or a blob is not checked, under either version: rev-list does not
list one, and git's version 0 check, which runs the same rev-list, lets it
through as well. A partial clone fetches the objects its filter left out
by their ids, and whether a ref reaches a given tree or blob is a walk of
every tree of every ref, which git itself does not make. Object ids are not
guessable, and git does not treat reachability as a boundary between
readers of one repository (see the SECURITY section of
`gitnamespaces(7)`).

## The hub's fork view names no change yet

The package builds the fork view of `parleyd` "from packages 15 and 22":
the tracking names package 15 records on a fork, and the changes whose
source is the fork, which package 22 records. Package 22 (the change
feature, `internal/change`) is not part of the hub yet, so the fork view
installed in `internal/hub/gitserver.go` names the fork's tracking refs
from its record (`fork.BySlug`: `refs/heads/<b>` for each tracking branch,
`refs/tags/<t>` for each tracking tag) and no change. A fork therefore
advertises no version ref, and the copy deletes every
`refs/parley/changes/` ref from the fork's repository. The copy of the
version refs of named changes is in place and tested through the test seam
(`AdjustGitServer`) with a fork view that names changes; once package 22
records a change's source, the hub's view names those changes and nothing
else changes.

## The copy takes namespaces as well as names

The package's copy function "copies named refs" and deletes "each named ref
the source no longer has". The version refs of a change are not known by
name before the parent's refs are read, and the fork's version refs of
changes it does not name must go, though the parent has them. So
`gitrepo.CopyRefs` takes, besides `Names`, `Prefixes` (every ref of the
source below `refs/parley/changes/<number>/` of a named change is set) and
`Prune` (every ref of the fork below `refs/parley/changes/` that the copy
does not set is deleted). Names, prefixes and the prune namespace come from
the git server alone, so no other ref of the parent is copied and no other
ref of the fork is touched.
