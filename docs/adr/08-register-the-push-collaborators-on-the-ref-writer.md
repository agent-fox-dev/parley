# 08. Register the push collaborators on the ref writer

## Status

Accepted.

## Context

Every write to a workspace's refs, whatever the surface (a push over git,
a commit through the source control API, a land), goes through one rule
engine and one ref writer in `internal/gitserver/pushrule`. Both take part
in work that other packages own and that are built after this one, on top
of it: the version a change gets when its source branch moves and the note
that a target moved are ref-updated stages of packages 22 and 23, the
decision for a carry-patch workspace is package 37's controller, the files
of a version without new commits come from a lookup of packages 22 and 29,
a fork's last activity is package 16's recorder, the head_sha of a
workspace is written by package 10's store, and the listeners told after a
write are packages 37's and others'. `pushrule` cannot import any of them;
they hand it what it calls.

As for the identity removers of ADR 03 and the teardown collaborators of
ADR 07, a package-level registry would be shared by every hub in a test
binary: a test that registers a failing stage would make the pushes of
every other test beside it fail.

## Decision

The collaborators are registered on the instances that call them, never in
a package-level registry:

- On the `*pushrule.Writer`: `RegisterRefUpdatedStage(RefUpdatedStage)`
  adds a stage after those before, the stages running in registration
  order in the store transaction of every accepted request;
  `RegisterForkActivity(ForkActivity)` sets the one fork activity
  recorder, replacing the one before, nil removing it;
  `RegisterPostPushListener(PostPushListener)` adds a listener after those
  before, each told of every accepted write, on a goroutine of its own, by
  the writer's event step `Finish`.
- On the `*pushrule.Engine`: `RegisterController(Controller)` sets the one
  carry-patch controller, replacing the one before.
- On the `*gitserver.Receiver`, the push processor behind
  git-receive-pack: `RegisterOptionsStage(OptionsStage)` and
  `RegisterMappingStage(MappingStage)` set the one options stage and the
  one mapping stage, each replacing the one before, nil removing it.
- What the hub itself provides rather than a feature (the store's
  transaction, the event log, the opening of a workspace's repository,
  the head_sha setter, the settings reader, the tracking
  lookup, the version-files lookup, the logger) is handed over with
  `Writer.Bind(WriterDeps)`, `Engine.Bind(EngineDeps)` and
  `Receiver.Bind(ReceiverDeps)` in a start hook.

Registration is safe from several goroutines and happens while the hub is
composed, before any request. Each call of the engine, the writer or the
processor (`Evaluate`, `Apply`, `Finish`, a push) takes the collaborators
registered then once, at its own start, and calls them for the rest of
that call: `Finish` takes the post-push listeners when it starts, not
when `Apply` did. The zero values are valid: with nothing registered, the engine refuses a `carry_patch`
workspace's updates as internal rather than allowing them, and the writer
runs no stage, records no activity and tells no listener, and the
processor accepts every
push option and adds no line.

The hub creates the ref writer in `newComposition`, so that it exists from
the composition on and a part composed before the git server, the forks
part in `internal/hub/fork.go`, can register the fork component as its
fork activity recorder; `(*Composition).RefWriter()` returns it, and the
git server's start hook in `internal/hub/gitserver.go` binds it. Every
other package that takes part in a write registers its collaborator on
`c.RefWriter()`, the engine (`c.PushRules()`) or the processor
(`c.Receiver()`), all three created in `newComposition`, from its own part
in `internal/hub`.

## Consequences

- Each hub, and so each test hub, has its own writer and engine; a test
  registers stubs on those of its hub, through `hubtest.Config.Compose`,
  and affects nothing else.
- A package that takes part in every write adds nothing to `pushrule`: it
  writes one stage, controller or recorder and registers it, and is then
  called for every surface, since the git server, the source control API
  and landing all write through the same `Apply`.
- A stage's error stops the request: the refs do not move and nothing any
  stage wrote is kept, so a stage that fails makes every write to the hub
  fail until it is fixed.
- Registering on the writer does not depend on the part order of
  `internal/hub/hub.go`: the writer exists before any part runs, so a
  part may register on it wherever it is in the order.
