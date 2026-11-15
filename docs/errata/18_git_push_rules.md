# Errata: 18_git_push_rules

Where the implementation of package 18 (`.specs/18_git_push_rules`)
differs from what the package says, or decides what it leaves open.

## An engine without a settings reader uses the settings' own defaults

The package has the engine read the effective `protected_branches` and
`protect.owner_push` through a settings reader, and gives each collaborator
whose package may be absent a default, without naming one for the settings
reader. A `pushrule.Engine` bound without a `SettingsReader` uses the
defaults of the settings themselves (`internal/workspace/settings.go`):
`protected_branches` holds the workspace's default branch (none when it has
no default branch) and `protect.owner_push` is true. An engine without a
reader therefore still protects the default branch, rather than protecting
nothing and failing open. The hub always binds a reader.

## `AllowedVerdict` takes a context and the repository view

The package names the land's verdict builder `AllowedVerdict(updates)` and
has it compute `forced` as `Evaluate` does. Forced asks whether an update's
before is an ancestor of its after, which only the repository can answer,
so the function is `AllowedVerdict(ctx, view, updates) (Verdict, error)`:
it asks `view.IsAncestor` for every update that is neither a create nor a
delete and returns its error, with no verdict, when that question fails.
Creates and deletes ask nothing.

## A ref name in the way of another counts as a moved ref

The package lists, as the mismatches of the ref transaction, a moved ref,
a create of an existing ref, an update or delete of a missing ref and a
ref another transaction holds. A create whose name another ref is in the
way of (`refs/heads/feat/x` beside `refs/heads/feat`) is none of these, and
`internal/gitrepo` reports it as `ErrRefNameConflict` in a `*RefError`.
`Writer.Apply` answers it as it answers a moved ref, with outcome
`ref_moved` and `ref changed since it was read; fetch and try again` on
that update: the client's picture of the repository is stale, and a fetch
shows it the ref in the way.

## The ref writer's answers carry an outcome and an error type

The package says a mismatch ends with "outcome `ref_moved`" and a failed
stage with every update refused as internal. `Writer.Apply` returns a
`Result` whose `Outcome` is one of `accepted`, `refused`, `ref_moved` and
`internal`, and the update a mismatch is about has the error type
`ref_moved` beside its message, so that the source control API can turn it
into `409 ref_moved` from the result alone. A verdict refused because a rule
could not be evaluated (error type `internal`) ends with outcome
`internal` rather than `refused`, so that the processor counts it as an
error.

## A writer without a store refuses a request that has stages

A `pushrule.Writer` bound without `WriterDeps.Transact` has no store
transaction to run the ref-updated stages in. With stages registered it
aborts the ref transaction and refuses every update as internal, rather
than moving refs whose stages did not run; with none it moves the refs.
The hub always binds `rt.Store.Tx`.

## A forward is waited for, and only its own limit is a time-out

The package has the ref writer cancel a forward that has not finished
within 120 seconds and refuse the request with `origin rejected push:
timed out`, and leaves open what a forward that returns late, or a request
that ends meanwhile, does. `Writer.Apply` runs each forward on the
request's goroutine and waits for it to return; a forward must end when its
context does, as one running git through `internal/gitrepo` does. When its
limit (`WriterDeps.ForwardLimit`, default `pushrule.DefaultForwardLimit`,
120 s) has passed by the time it returns, the update is refused as timed
out with `patch_branch` even if the forward reports success, since it did
not finish within the limit. When the request's own context ends during a
forward (the push's `[git].timeout`, or the client gone), the request is
refused as `internal` and `Apply` returns the context's error, as for any
other step that the request's end stops.

## A command list the hub cannot read is answered `400`

The package answers every push `200` with a status report. A request whose
command list is not what git sends (a pkt-line that is not one, a command
that is not `<old> <new> <ref>` with forty-digit ids and a ref under
`refs/`, a create and delete at once, a gzip body that does not decode) has
no ref the report could name, so the processor answers it
`400 invalid receive-pack request` in plain text, as the git server answers
its other refusals, and the request counts as `error`. Stock git never
sends one.

## `report-status-v2` is answered as `report-status`

The receive-pack advertisement, which is git's own (package 17), offers
`report-status-v2`, and stock git asks for it instead of `report-status`.
Version 2 differs only by the `option` lines a server adds after a ref it
rewrote; the hub rewrites no ref, so the processor sends the same report for
either capability.

## A failure of the hub's before the pack is stored is reported per ref

The package words `internal error; try again` for a rule that cannot be
evaluated and for a failed ref-updated stage. The processor reports the same
text for a failure before the rules run (the options stage returns an
error, the repository cannot be opened, the quarantine cannot be made):
`unpack ok`, `ng <ref> internal error; try again` and an
`error: <ref>: internal error; try again` side-band line for every ref, and
the request counts as `error`.

## The options stage can mark a push as opening a change

The package gives package 19 an options stage that receives the push
options and may refuse the push or set its session. Rule 5 holds an update
without new commits to the files of the version it makes, which depends on
whether the update opens a change (`Update.OpensChange`), and only the
options say so. `gitserver.OptionsAnswer` therefore also has
`OpensChange`, which marks every update of the push; without an options
stage no update opens a change.

## A `before` that names no object is refused as internal

The rules are evaluated on the `before` the client sent. A `before` that
names no object of the repository fails the ancestry question of a
non-create, non-delete update, so the update is refused with
`internal error; try again` rather than with the compare-and-swap's
`ref changed since it was read; fetch and try again`. A ref cannot point at
an object the repository lacks, so only a client that invents its `before`
meets this; stock git sends the value it read from the advertisement or its
own remote-tracking ref, which the repository holds unless it was pruned.

## The event step is `Writer.Finish`, run with the result of `Apply`

The package gives the ref writer a second step that the caller runs after
the ref transaction, without naming it. It is
`(*pushrule.Writer).Finish(ctx, result)`, given the `Result` `Apply`
returned, which carries what the step writes; a result that is not
accepted writes and tells nothing. Its transaction runs to its end even
when the caller's context has ended, since the refs have moved. A caller
of `Apply` that never runs `Finish` leaves the request's journal entry in
place, so its events are written at the next start rather than lost.

## A journal entry that cannot be written refuses the request as internal

The package has the journal entry written in a transaction of its own
before the ref transaction and does not say what a failure of that
transaction does. It refuses every update with `internal error; try
again` and moves no ref, so no request moves refs whose events could be
lost; forwards to origin that already ran are left as they are, as for a
later failed forward. A writer bound without an event log keeps no
journal and writes no event.

## Reconcile writes the events of a committed entry whatever its refs hold

The package has the start-up reconciliation write the events of an entry
whose refs hold their after values and drop one whose refs were never
written. Reading the refs alone cannot tell an entry whose request moved
its refs, whose `Finish` then failed while the hub served on, and whose
refs a later accepted push moved again, from one that never moved them;
that request's events would be lost. The journal entry is therefore
marked `committed` inside the store transaction the ref-updated stages
run in, which commits right after the refs move, and Reconcile writes the
events of a committed entry and removes it whatever its refs hold now.
An entry not marked keeps the rule of the package: its refs all at their
after values mean the hub stopped between the commit of the refs and that
of the store (the gap the package accepts), and its events are written;
otherwise it is removed without events. A ref of an unmarked entry that
holds neither its before nor its after value can then only have been
moved by another writer after the request was refused (or, in that gap,
by a writer outside the hub before it started again); the entry is
removed without events as well, and logged as a warning. An entry whose
repository cannot be opened or read, or whose transaction fails, is
logged and kept for the next start rather than failing the start; the
start fails only when the journal itself cannot be read.

## A push's `[git].timeout` runs from its arrival to the preparation of its ref transaction

The package bounds every stage of a push before its ref transaction by
`[git].timeout` and says the push is finished once that transaction has
started. The push shares the request's one deadline, `[git].timeout`
after the request came in, with the git server's checks and a task fork's
ref copy, as a fetch does (see the erratum of package 17); the processor
has no limit of its own, so its limit is the git server's
`Deps.Timeout`, read with `gitserver.TimeoutFrom`. The transaction counts
as started once `git update-ref` has prepared it: `Writer.Apply` runs the
forwards, the journal entry and the preparation under the caller's
context, and everything after on one that does not end, for every surface.
When the context ends before, `Apply` refuses the request as internal, as
the forward section above says, and the processor reports that push, like
one stopped in an earlier stage, as `unpack push timed out` with
`ng <ref> unpacker error`. A client that went away is treated the same,
though it reads no report. A deadline that passes while the command list
is still being read leaves no ref to report on, so that request is
answered `400 invalid receive-pack request`.

## `[git].max_push_size` counts every byte after the commands and options

The package bounds the pack. The processor counts every byte of the body
after the command list and the push options, after gzip decoding, so
whatever a client sends after its pack counts too, and it stops reading at
one byte over the limit: also when it reads the rest of the body only to
discard it, after the options stage has refused the push or for a push of
deletes only, which is then answered as before. The key is a string, like
`[server].max_body_size`: `"0"`, a whole number of bytes, or a whole number
followed by `KB`, `MB` or `GB`.

## A path-scoped token is held to the trees and blobs a ref is pointed at

The package holds a path-scoped token to the commits a request adds and
to the files of a version an update without new commits makes. A ref
other than a branch may also be pointed at a tree, a blob, or an
annotated tag of one, which adds no commit and makes no version; such an
update would bring files outside the token's paths into the repository
unchecked. An update that adds no commit is therefore held by what its
after value reaches through any tags (`View.ObjectPaths`): a commit as
before, by the files of its version; a tree by every path in it, as the
tree of a root commit is; and a blob, which has no path the token's
globs could allow, is refused for every path-scoped token with
`path_not_allowed`, the ref's name as the path:
`path <ref> is outside this token's allowed paths`. Refusing it as
`internal` would tell the client to try again, which can never succeed.

## A push stopped after its objects moved in has them taken out again

The package moves a push's quarantined objects into the repository once
the refs are locked, before the stages run, so that the stages read them,
and says a refused push leaves none of its objects behind. A request that
stops after that move (the store transaction cannot begin, a stage
fails, the refs cannot be committed) moves no ref, and its objects would
stay in `objects/pack`. `Quarantine.Migrate` therefore notes the files it
adds, and `Quarantine.Unmigrate` removes them again, each pack's index
first; `Writer.Apply` runs it through the `ApplyRequest.AfterAbort` hook
whenever the request stops with no ref moved after `BeforeCommit` ran.
`Migrate` hard-links each file into place and leaves a file the
repository already has as it is, so it never notes, nor `Unmigrate`
removes, a pack that was there before. Two concurrent pushes whose packs
are byte for byte the same share one file, and if the push that added it
is stopped, the file goes; git names a pack for its content, so this
needs two clients to build the same pack at once, and the hub accepts
that.

## A ref transaction waits for the packed refs, and a lock on no ref of it is git's error

A transaction of `git update-ref` that deletes a packed ref holds the
lock of `packed-refs` from its prepare to its end, and git waits one
second for that lock by default, so a second transaction deleting another
packed ref while the first runs its stages failed, and was answered as a
moved ref on its first update, which never moved. `PrepareRefs` runs git
with `core.packedRefsTimeout` at 10 seconds, so the second waits for the
first to end. When the prepare still fails on a lock held on none of the
transaction's refs, such as that of `packed-refs`, no ref moved, and the
failure is git's error, which the ref writer answers as internal; only
the lock of one of the transaction's own refs counts as a moved ref,
naming that ref.
