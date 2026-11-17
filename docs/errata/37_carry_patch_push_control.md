# Errata: 37_carry_patch_push_control

Where the implementation of package 37 (`.specs/37_carry_patch_push_control`)
differs from what the package says, or decides what it leaves open.

## A forward pushes from the repository the update is written to

The package has the forward push "the update's after commit" from the hub's
repository, as if the commit were there already. For a git push it is not.
The objects of a push stay in its quarantine (`objects/incoming-*`,
`internal/gitrepo/quarantine.go`) until the ref writer's `BeforeCommit`
moves them in, and the ref writer runs the forwards before that
(`pushrule.Writer.Apply`, erratum 18). A push from the hub's copy would not
find the commit.

So the ref writer hands each forward the repository the request writes to,
`ApplyRequest.Repo`: for a git push the view of its quarantine, which reads
the quarantined objects through `GIT_ALTERNATE_OBJECT_DIRECTORIES`.
`pushrule.Forward` is `func(ctx context.Context, repo *gitrepo.Repo) error`.
`(*gitrepo.Repo).Push` runs `Git.Push` from a repository, with that
repository's git environment, so the view pushes what only the quarantine
holds. The carry-patch `Forwarder` is
`Forward(ctx, repo, w workspace.Workspace, branch, commit)`:

- `repo` is the repository to push from. `nil` means the hub's copy of the
  workspace, for a caller whose commit is already there, such as the landing
  guard, whose replayed head is in the target repository.
- `w` is the workspace record as the decision read it, not a slug. The
  forward needs its `git_url`, and the controller and the landing guard
  have both just read the record, so the forward does not read it again.
  The origin credential is still read at the forward (`ResolveOriginCredential`).

## A forward records origin's head only of a commit the hub's copy holds

The package records the forwarded commit as the branch's origin head after
every successful forward. The record is the ref
`refs/remotes/origin/<branch>` of the hub's copy. For a git push the commit
is still only in the push's quarantine when the forward succeeds. If a later
step refuses the push (another branch's forward fails, or the ref
compare-and-swap does), the quarantine is discarded. A record written
through the view would then name an object the copy does not have: a
broken ref that `git gc`, `fsck` and a backup trip over.

So the record is written once the copy holds the commit:

- `Component.Forward` writes it right after a successful forward only when
  the copy holds the commit (`Repo.Missing`): a forward from the copy
  (`repo` nil, the landing guard), or an update whose objects are already
  in the copy.
- For a git push or a source control API write, the carry-patch post-push
  listener writes it after the write is accepted, when the copy holds the
  pushed objects: for each accepted update whose decision carried a
  forward and that is not a delete, it records the update's `after` as
  origin's head of the branch. It does so only while the hub's branch is
  still at `after`, and under the workspace's trigger lock: when a later
  write has already moved the branch, that write's listener, which runs
  after it, records the newer commit, so an older commit never overwrites
  a newer record.

Between the forward and the listener the record still holds the older
head. Nothing reads it in between: for a patch branch whose authority is
origin, the record is read by the next sync's origin refresh, which fetches
origin's real head first (erratum 36), and neither the mirror (authority
hub) nor package 13's forced push (refused for patch branches) uses it as a
lease. A failed record write is logged and the write stands, since origin
holds the commit.

## Origin's own message is what its hooks wrote

For an update origin refuses, the package's message is
`origin rejected push: <origin's own message>`, with `branch is frozen` for
a hook that wrote that text. `gitrepo.PushError.Reason` is the status reason
followed by the hook's text (`pre-receive hook declined: branch is frozen`).
So `PushError` has a field of its own, `OriginMessage`: what origin's hooks
wrote, the `remote:` lines, one per line. The forward uses it when it is not
empty and falls back to `Reason` when origin wrote nothing. Like every
forward message, the whole text is then scrubbed of the credential and of
URL user information, and cut to 1024 bytes.

## A forward to a workspace without an origin fails as `push failed`

A carry-patch workspace whose record has no `git_url` cannot be forwarded
to. The package names no reason for it, so the forward fails with the
reason for any other failure, `origin rejected push: push failed`, without
reading the credential or running git.

## A caller without the owner exemption is the owner with a bound PAT

The package's check of a protected patch branch pushes as "an org member
with write access and a PAT carrying git:write". On a repository workspace
there is no such member: the access rule gives an org member read-only
access, and members contribute through task forks (a member's push is
`403` before any push rule runs). The callers that can write a repository
workspace and still lack the owner exemption are the owner with a PAT
bound to the workspace or limited to paths (package 18's exemption covers
only an unconfined credential of the owner or the admin token), and the
owner while `protect.owner_push` is false. The test pushes the member rows
as the owner with a PAT bound to the workspace, which the protected-branch
rule refuses with the same messages before the controller is asked.

## The hub keeps the controller it registers

`Composition.PushControl()` returns the carry-patch controller the hub
registered with its push rules. The engine has no getter for its
controller, and a test that counts the controller's decisions must wrap the
one the hub registered, not one of its own, or it would pass whether the
hub registers one or not. A test wraps it from `hubtest.Config.Compose`,
which runs after the hub's parts, and registers the wrapper with
`PushRules().RegisterController`, which replaces it.

## A trigger ends when the hub stops

The package runs the triggers after the client's answer and says nothing of
a stop. The ref writer starts each post-push listener on a goroutine of its
own with a context that never ends, and the hub's stop does not wait for
it; a trigger may wait for the one before it and, once the mirror runs,
push for up to 120 s. So the hub registers the listener with a context
that ends when the stop begins (`Composition.ShuttingDown`): a trigger
still waiting for its workspace's lock does not run, and an enqueue or git
cut off by the stop fails instead of using the stores after they close.
A trigger cut short this way is lost and logged; a rebuild can be queued
by hand after the restart. A mirror cut short this way writes no
`parley.patch.mirror_failed` event and pushes no further branch: the stop is
not origin's failure. The next accepted update of the branch mirrors it
again.

## A mirror reports each branch it could not push

The package resolves the origin credential once per mirror run and wants
one `parley.patch.mirror_failed` event per failed branch, but the check of
that rule asserts a single event in every row of its table, the row with an
unreadable credential included, although that request updates patch/a and
patch/b. The requirement decides: every branch the run would push and could
not gets its own event. With an unreadable credential each such branch is
reported with `failed to resolve origin credentials`, each one origin
cannot be reached for with `origin unreachable`, and the test asserts two
events in both rows. A branch the hub's copy no longer has is skipped and
reported by nobody, whatever else failed. A workspace without `git_url`
reports each branch with `push failed`, as the forward does.

## A mirror reads the heads once, at its start

The package has the mirror read each branch's head "now", just before its
push. The mirror reads the heads of all the branches it pushes, and the
origin heads the hub's copy records for them, in one git at its start, not
two per branch: the mirror runs after every accepted update of a patch
branch, and each git costs. A branch that moves while an earlier branch of
the same run is pushed is pushed by the trigger of the request that moved
it, which runs after this one under the same per-workspace lock, so origin
still ends at the hub's last head. A successful push whose head is already
the recorded origin head writes no record.
