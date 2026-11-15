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
client gets git's reason and never a path of the hub. A line that still
names an absolute path after that replacement, such as the object directory
of a fork's parent that git reads through the fork's alternates, is replaced
as a whole by `upload-pack failed`. The `ERR` pkt-lines
the hub writes carry no trailing newline, as git's own do.

## A want of an object no ref reaches is refused by the hub, not by git

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

Nor does git check a tree, a blob or a tag under either version: a want of
a blob no ref reaches, sent with `uploadpack.allowReachableSHA1InWant=true`,
is answered with a pack that holds it under version 0 as under version 2.
That would let any reader of a workspace download, by its id, an object of
rewritten history, and a PAT bound to a task fork the objects of the parent
that the fork's own refs do not reach, which the fork's repository reads
through its alternates.

So the hub refuses them itself (`internal/gitrepo/serve.go`, `checkWants`),
under both versions: a version 0 request is kept in a file of git's private
directory up to the flush packet that ends its wants, its `want` lines read
by their leading hexadecimal digits as below, and git reads that file and
then the rest of the request as it comes. A version 2 request is kept in
such a file whole and read
as git's version 2 server reads it, the command and the capabilities in any
order up to the delimiter packet (a capability line such as `agent=` may
come before `command=fetch`); for a `command=fetch`, every `want` id is
read as git reads it, the leading 40 (SHA-1) or 64 (SHA-256) hexadecimal
digits, in either case, whatever follows them, so that `want <ID IN
UPPERCASE>` and `want <id> junk` are checked as the id git serves; both
lengths are given, lowercased, to
`git rev-list --ignore-missing --stdin --not --all`, which resolves the one
of the repository's object format and leaves out a name it cannot resolve.
When that lists a commit, an annotated tag's commit included, the request is
answered with the `ERR` pkt-line `upload-pack: not our ref <id>`, git's own
text of version 0, before upload-pack runs. Otherwise the wanted ids are
given to `git cat-file --batch-check`, and every tree, blob and tag among
them that no ref points at is looked for among the objects the refs reach,
`git rev-list --objects --in-commit-order --use-bitmap-index --all`, read
as it comes and stopped once every one has been met; one it never meets is
refused with the same text. A version 2 `want` without 40 leading
hexadecimal digits, which git cannot read, is refused the same way with
git's text `upload-pack: protocol error, expected to get oid`; under
version 0 git refuses such a want itself. An object no ref reaches (a
commit whose branch was deleted, a blob of rewritten history, an object of
the parent that a task fork can read only through its alternates) is
thereby served under neither version.

A fetch of commits, also of annotated tags that refs point at, as every
clone and fetch by ref sends, costs two short git commands and no walk. The
walk is made for a tree, a blob or a tag no ref points at, which is what a
partial clone wants when it fetches the objects its filter left out: every
object a ref reaches is met, so that fetch is served, and it costs up to one
walk of the repository's objects, the trees and blobs of each commit after
it, newest first, so that the objects of a recent checkout are met early.
Reachability bitmaps, where a repository has them, answer in place of the
walk.

## A request is kept up to 16 MiB, answered `ERR request too large` beyond

The package takes a fetch body "of any size". The want check above keeps a
version 2 request whole on disk before git runs, and a version 0 request up
to the end of its wants, so the hub bounds what it
keeps: up to `gitrepo.DefaultMaxRequest`, 16 MiB, counted after gzip
decompression. A fetch request is a pkt-line of about 50 bytes per want and
per have; stock git sends a few thousand haves at most in one round of
negotiation, and a fetch of 100,000 refs is about 5 MB of wants, so a real
request stays far below the bound. A larger version 2 request, or a
version 0 request whose wants run past the bound, is answered
in band, before git runs, with the `ERR` pkt-line `request too large`, a
text of the hub's beside the package's `empty request`, `invalid gzip body`
and `request timed out`, and counted with the result `error`. The haves of
a version 0 request, after its wants, are not kept but streamed to git, and
have no such bound.

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

## The hub installs no push processor yet

The package passes "the push processor of package 18" to the git server.
Package 18 (the push rules) is not part of the hub yet, so
`internal/hub/gitserver.go` installs none, and `cmd/parleyd` is unchanged,
as for every other collaborator of the git server. Ref discovery for a push
is served, and a push that passed every check is answered
`500 internal error` with its body unread and git not run, so no ref of the
repository changes and stock `git push` fails. The hand-over to an
installed processor is tested through the test seam (`AdjustGitServer`)
with a processor that records what it was handed. Once package 18 sets
`gitserver.Deps.Push` in the git server's start hook, pushes reach it and
nothing else changes.

## The PAT a push processor gets is the principal of the credential check

The package hands the processor "the PAT's record with its paths". The
credential check returns an `identity.Principal`, which holds a PAT's
`token_id`, user, permissions, `paths` and bound workspace: what the push
rules read of a PAT. `gitserver.PushCaller.PAT` is that principal for a
PAT, and nil for an API key or the admin token, rather than the stored
`identity.PAT` row, which the git server never reads. The admin token's
credential type is `admin_token`, the name the hub's events give it, where
the principal says `admin`.

## The event log is passed in `internal/hub`, as every collaborator is

The package passes "the event log of package 52 as the sink in
`cmd/parleyd`". As with the other collaborators, the git server's start hook
in `internal/hub/gitserver.go` sets `gitserver.Deps.Events` to the hub's
event component (`Composition.Events()`), and `Deps.Settings` to the
workspace service's settings registry, from which `audit_fetches` is read;
`cmd/parleyd` is unchanged.

## The filter of a fetch event is the text git sends

The package records "the request's filter text", and its check expects
`blob:limit=1k` for `git clone --filter=blob:limit=1k`. git normalises a
size limit to bytes before it sends the request (`filter blob:limit=1024`),
so the hub never sees the unit its user typed; the event records the text of
the request's `filter` line, `blob:limit=1024`, and the test expects that.
A filter line longer than 1024 bytes is recorded cut to that length.

## "No `audit_fetches` key registered" is a registry without the key

The settings registry of the workspace feature always holds
`audit_fetches`, so a hub in which the key is not registered cannot be
built. The git server reads the setting through its `Settings` dependency,
and the case is tested with an empty `workspace.SettingsRegistry` set
through `AdjustGitServer` while the stored settings document holds
`audit_fetches: true`: no event is written.

## The credential helper refuses every hub URL the other commands refuse

The package lists the settings for which `parley credential-helper get`
answers nothing: a missing hub URL or credential, a hub URL without a host
or of a scheme other than `http` and `https`, and a configuration file that
cannot be read. Its design decision says the helper resolves its settings
as every command does. It does so literally: it calls `resolveSettings` in
`internal/cli/config.go`, the function every command of auth `user` or
`admin` resolves its settings with, and takes any error it returns as no
answer. That function also refuses a hub URL with a query or a fragment
(`checkHubURL`), with which no other command works either, so the helper
answers nothing for such a URL too, where the package's list would answer
when the scheme, host and port match.

## A fork's `HEAD` is set by the git server

A fork's repository is made by package 15 with `git init --bare` and no
`HEAD` of its own, so it names git's default branch name, which the fork
does not have, and a fork advertised no `HEAD`: a clone of a fork checked
nothing out, and a single-branch clone (`--depth`) took no branch at all.
The copy that brings a fork's refs up to date before git runs
(`gitrepo.RefCopy.Head`, set in `internal/gitserver/forkview.go`) also
points the fork's `HEAD` at `refs/heads/<default_branch>` of the fork's
record, its base branch, when it names another ref, so that a fork is
cloned as a workspace is, with its base branch checked out.

## `[git].timeout` bounds one request, the fork's ref copy and git together

The package says `[git].timeout` "bounds one upload-pack request". Before
git runs on a task fork, the git server copies the refs the fork shows of
its parent's into the fork's repository, and two requests on one fork wait
for each other's copy. One deadline, `[git].timeout` after the request came
in, bounds the copy, the wait for another request's copy (the 64 fork locks
are channels a request waits on no longer than its deadline) and the run of
git, so that a copy that hangs holds a request, and every request behind it
on the same lock, no longer than the timeout. A request whose deadline
passes during the copy or the wait is answered `500 internal error`, before
git runs; one whose deadline passes while git runs gets
`ERR request timed out` as before.

## A fork is audited by its parent's `audit_fetches`

A task fork has no settings of its own: its workspace object shows its
parent's, read at the time of the request, and an update of a fork's
`settings` is refused. The git server therefore reads `audit_fetches` of a
fork from its parent's record (`internal/gitserver/event.go`
`auditsFetches`); the event still names the fork. When the parent's record
is not found no event is written, and when it cannot be read the failure is
logged and no event is written, since the fetch has been served.
