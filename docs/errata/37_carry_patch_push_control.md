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

So after a successful forward, `Component.Forward` writes the record only
when the copy holds the commit (`Repo.Missing`). That is the case for a
forward from the copy (`repo` nil, the landing guard) and for an update
whose objects are already in the copy. For a git push the record is left
as it was. The hub does not lose anything by it: for a patch branch whose
authority is origin, the record is read by the next sync's origin refresh,
which fetches origin's real head first (erratum 36). Neither the mirror
(authority hub) nor package 13's forced push (refused for patch branches)
uses it as a lease. A failed record write is logged and the forward stands,
since origin holds the commit.

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
