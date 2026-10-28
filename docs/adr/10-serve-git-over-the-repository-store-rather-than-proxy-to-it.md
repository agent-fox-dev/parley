# 10. Serve git over the repository store rather than proxy to it

## Status

Accepted.

## Context

Each workspace's repository is served at the hub's own address, over git
smart HTTP. The store behind it, Cloudflare Artifacts, speaks the same
protocol and has a remote of its own, so the obvious implementation is a
byte proxy: take the client's request, mint a repo-scoped token, pass
the bytes through.

Two things rule that out.

The contract requires the hub to decide **each ref of a push before any
of it reaches the store**: a patch branch refused while the ordinary
branch of the same push lands, and nothing under `refs/hub/replaced/`
touched by any client on any workspace. A proxy cannot do that, because
by the time it knows what the push contains it has already sent it.

And the store's remote exists only on Cloudflare. The second target has
a repository on its own disk and no HTTP remote at all, so a proxy would
be one target's implementation rather than the service's.

## Decision

**Parley speaks the git server protocol itself, over the
`RepositoryStore` interface.** The advertisement is built from
`listRefs`, a fetch is `servePack(wants, haves)`, a push is parsed into
ref commands, filtered, and handed to `receivePack`. Both targets serve
git the same way, and the ref-level control the contract asks for falls
out of the design rather than being bolted onto a byte stream.

**The pkt-line framing is the hub's own.** isomorphic-git exposes no
pkt-line encoder or decoder — package 05 established that and wrote
`src/git/pktline.ts` from the protocol documentation for the client side
(ADR 2). This package reuses it for the server side. That is the finding
the specification asked this package's first task to record: nothing in
the installed libraries offers the advertisement, the receive-pack
command list or the per-ref report.

**A store token never leaves the service.** It is minted for the call
that needs it and reaches no response, header or log; the only address a
client is given is the hub's.

**Every refusal is pkt-line, including the ones nobody expected.** A git
client renders a pkt-line `ERR` to the person and reports the service's
JSON envelope as a server it could not understand, so the git routes
authenticate themselves rather than going through the middleware that
answers in the envelope, and a failure that escapes a handler is framed
too.

## The protocol version the hub speaks

**Version 0, and a client that asks for version 2 falls back to it.**
That is what `docs/prd/api.md` describes, and the measurements say it is
the right way round rather than a shortfall.

Version 2's negotiation cannot be served statelessly. Measured against
the real binary, on a repository of forty commits with one new commit to
fetch:

| What the service answered a round with        | What git did next                               |
| --------------------------------------------- | ----------------------------------------------- |
| `acknowledgments`, `NAK`                      | sent `done` **with no haves** — a full transfer |
| `acknowledgments`, `ACK`×6, `ready`, the pack | sent `done` with no haves — a full transfer     |
| `acknowledgments`, `ACK`×6, no `ready`        | sent `done` with no haves — a full transfer     |
| the version 0 advertisement                   | one round, six haves, a 234-byte pack           |

In version 2 a client sends its haves once and expects the server to
remember what it acknowledged; an HTTP service that keeps no state
between requests has nothing to remember with. In version 0 the client
resends every have each round, so one round narrows the pack. Serving
version 2 natively would mean keeping negotiation state somewhere both
targets can reach, which is a cost this package does not need to pay to
do its job.

## Consequences

- The hub implements a git server, and a protocol detail it gets wrong
  is a clone that hangs rather than a field that is missing. The
  integration suites drive the real `git` binary against the real routes
  for that reason.
- Two of those details are already paid for and worth naming, because
  both cost an afternoon: a 401 without `WWW-Authenticate` makes git
  report "Authentication failed" without ever offering the credential it
  holds, and a test that drives git against an in-process server with a
  **synchronous** child process deadlocks — the server cannot answer
  while the event loop is blocked.
- `servePack` is the whole of the hub's fetch negotiation, so how well a
  fetch narrows to what the client lacks is the store's business, not
  the route's. The non-Cloudflare store answered every fetch with the
  whole repository until this package; it now walks what the wants
  reach and leaves out what the haves do, which is also why a result
  the engine keeps under one ref has to be referenced by whoever goes
  on using it.
- Nothing stops a later version proxying the parts that need no
  decision, if the measurements ever justify it. The ref-level
  operations could not be among them.
