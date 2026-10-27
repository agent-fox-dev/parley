# 5. Hold the workspace lock as a lease in the relational store

## Status

Accepted.

## Context

Operations that work on a workspace's repository — sync, reclone, archive,
rebuilds, merges, branch pushes — must take turns. A request that finds
another running answers `409 workspace_busy` at once rather than waiting.
Three properties follow from the reference: the exclusion holds across
**every** instance of the service, not inside one; a taker never waits; and
an instance that stops without releasing the lock must not leave the
workspace busy for ever.

The obvious Cloudflare answer is a Durable Object per workspace: one actor,
serialised by construction. It has two problems here. It is a second,
Cloudflare-only mechanism beside the store that already holds every other
fact about the workspace, so the non-Cloudflare target would need an
unrelated implementation with its own failure modes. And nothing this
repository can run — not `make check`, not the conformance harness without a
deployment — can exercise it, so the mutual exclusion would be asserted
rather than measured.

## Decision

The lock is a row in the relational store with a lease:

```
workspace_locks(key PRIMARY KEY, holder, taken_at, expires_at)
```

Taking it is one statement — an insert that, on conflict, updates **only
when the lease has run out** — followed by a read of the holder. A caller
whose own holder id is in the row has the lock; anybody else gets
`undefined` and the route answers `409 workspace_busy`. Releasing deletes
the row, and only for the holder named in it, so a lease that has already
passed to somebody else is not torn away from them.

One implementation serves both targets, over the store each is wired with.
This follows [ADR 1](01-define-the-platform-interfaces-and-their-implementations.md):
some platform facilities have a genuine implementation per platform, and
some have one that both share.

## Consequences

- The exclusion is as shared as the store is, which is exactly the scope the
  reference asks for, on both targets and with no second mechanism.
- An instance that disappears holds the lock for at most one lease (60
  seconds by default) and the workspace frees itself with nobody
  intervening.
- Taking the lock costs a write and a read. Operations that take it already
  talk to a git remote, so this is noise against them.
- The contract suite runs against both store implementations, so the
  behaviour is measured on each rather than argued for.
- A long operation must outlive its lease or renew it. Nothing yet runs
  longer than a lease; the first operation that does needs a renewal, and
  that is a decision of its own.
- A Durable Object implementation can replace this one behind `Locks`
  without a caller changing, if a deployment ever wants the actor.
