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
