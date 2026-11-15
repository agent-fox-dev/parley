# Errata: 19_git_push_to_change

Where the implementation of package 19 (`.specs/19_git_push_to_change`)
differs from what the package says, or decides what it leaves open.

## Command order is the order git sends the commands in

The package has the lines of a push follow its command order, and its
check expects `git push -o parley.change origin b a refs/tags/v1`, where `a`
already has a change and `b` is new, to print the line of `b` before the
line of `a`. Stock git does not send its commands in the order of its
command line: it sends the commands of the refs the hub advertised first,
in the advertised order, and those of new refs after them, so that push
sends `refs/heads/a` before `refs/heads/b`. The mapping takes the branches
in the order the commands arrive (`ChangeMapper.Map`), which is the order of
the status report as well, and the line of `a` comes first. The test reads
the order from git's packet trace and checks the lines against it.

## A branch that already has a change gets its version from the ref writer

The check of "the same options on every open of one push" expects a
record-version call through the mapping's change service for the branch
that already has an open change. The package's own decisions say
otherwise: the ordinary version of a push is recorded by the change
feature's ref-updated stage inside the ref update, and the mapping reads
it from the ref writer's result (`pushrule.UpdateResult.Version`); the
service's record operation is used only after a race to open (see
*A push whose change was opened after it moved the branch*). The
unit test gives that branch a version the ref writer reported, and checks
that the push opens changes for the other branches only, each with the
same options. A record-version request (`VersionRequest`) has no field for
a target, a title, an intent or a draft flag, so no version request can
carry them.

The check that a PAT without `changes:write` still adds a version to a
branch's open change expects a record-version call too. For the same
reason, that version is one the ref writer reported: the unit test gives
the PAT's branch a reported version and checks that it is the outcome,
with no call and no warning. The scope check runs only before an open,
and never after a lost race, because the open was already allowed then.

## A push whose change was opened after it moved the branch

The package records a version of kind `push` on the existing change after
an open refused with `change_exists`. When two pushes race to open the
change of one branch, the winner's open follows the branch (erratum 22,
*The open of a commit's change follows a later move of its branch*) and
may already have recorded the loser's head, as kind `ref` by the hub,
before the loser's mapping runs; the loser's mapping then finds the
change, or its open is refused. Both are answered the same way, in one
store transaction, on the change the lookup or the refusal named:

- a change that left draft and open since is the warning
  `change <workspace>#<number> is <state>`, nothing recorded;
- a change that already has the pushed head answers its current version,
  `change <workspace>#<number> version <n>`; a version the hub recorded
  on following the branch (kind `ref`, the system actor, no session)
  becomes the push's: kind `push`, created by its caller with their
  credential, in its session, which is linked to the change. The
  `parley.change.version` event written when the hub recorded it is
  left as it was, kind `ref` by the hub;
- a change whose branch a later write moved past the pushed head answers
  its current version and records nothing, since the later write's
  version stands and a version at the pushed head would no longer be the
  branch's head;
- any other head is recorded as the next version, of kind `push`.

A push whose found change has no version reported gets its line whatever
options it gave, as a push to a change's source does. An open whose branch
was deleted between the push and the open abandons the change it opened
(`source_deleted`), and the push is told
`warning: no change for <branch>: branch was deleted, change <workspace>#<number> abandoned`
instead of `(opened)`.

## A refusal with a server status is an internal error

The open's refusals are `*server.Error` values, and every one that is
not worded on its own is reported as its message followed by its error
type. The mapping takes only those below status 500 as refusals: a
`*server.Error` of status 500 or above is an internal error, like an
untyped one. It is logged, its text is never sent, and the reason is
`internal error`.
