# 2. Store repositories as refs and packs, over our own git client

## Status

Accepted.

## Context

Each workspace's repository is held in Cloudflare Artifacts, one repository
per workspace. The binding was read from the installed types
(`@cloudflare/workers-types`, experimental entry point) rather than from the
documentation, and three gaps in it shape everything built on top:

- **No write surface.** The binding creates, gets, lists, forks, imports and
  deletes repositories, and reads objects — `info`, `log`, `readCommit`,
  `readTree`, `readBlob`, `readFile`. Nothing writes a ref or an object.
  Every write goes over the git protocol, to the repository's `remote` URL,
  with a repo-scoped token the deployment mints.
- **No ref listing.** Nothing enumerates branches or tags; `log` and the
  readers all need a ref or a hash the caller already has. "Every branch and
  tag of the remote is present" can only be answered over the protocol.
- **No storage error.** The error codes run from `ALREADY_EXISTS` to
  `MEMORY_LIMIT` and `INTERNAL_ERROR`. None of them names the 1 GB cap on a
  repository, so a refusal that names the limit cannot be a translation of
  something the store said.

Two smaller facts follow from the same reading: a repository name may hold
only alphanumerics, dots, hyphens and underscores — no slashes, so no
`org/workspace` — and `import()` takes no credential and runs once, which is
why Parley copies every repository in itself (decision D15 of the port PRD).

Three capabilities the contract needs are undocumented and undecided
(decision D16): a ref outside `refs/heads/` and `refs/tags/`, a
non-fast-forward update, and a ref deletion. Packages 07, 08 and 09 are
designed differently depending on the answer.

Finally, decision D14 rules out shelling out to `git`: the deployment stays
serverless, with no container image and no second runtime.

## Decision

**1. The repository store interface is refs and packs**, not a mirror of the
Artifacts binding. It lists refs, updates refs, receives a pack and serves
one, and reads an object. On Cloudflare it is Artifacts: the binding for the
repository's lifecycle and for object reads, the `remote` URL and a minted
token for refs and packs. Off Cloudflare it is a bare repository written in
TypeScript over the blob store. One contract suite runs against both.

**2. Parley speaks git smart HTTP v2 itself.** No `git` binary and, in this
package, no `isomorphic-git`. A copy asks the remote for its refs, requests a
self-contained pack — Parley does not offer `thin-pack` — and relays that
pack into `receive-pack` with the ref updates the ref advertisement named.
Copy-in therefore decodes almost nothing and streams rather than buffers.
Object-level work starts in 06, where the history engine needs it, and the
library question is answered there.

**3. A copy is checkpointed by ref batch.** The default branch is copied
first, then the remaining refs in batches, each asking only for what the
earlier batches did not bring. The cursor lives in the job record, so a copy
that cannot finish in one unit of platform work continues in another instead
of starting again. This introduces the job-runner platform interface, which
ADR 1 did not define.

**4. The capability report is declared by each implementation and verified
by a probe.** An integration test and a conformance check exercise all three
capabilities against a scratch repository and fail when the declaration and
reality disagree. Probing at runtime was rejected: it costs a scratch
repository on a cold path, and a store that is briefly unwell gets its wrong
answer cached. The measured answers get their own ADR once the probe has run
against real Artifacts; until then no package may assume any of the three.

**5. A workspace's repository is named `ws-<workspace slug>-<short hash of
the workspace id>`.** Deterministic, inside the store's name charset,
collision-free across slugs that differ only in case or hyphens, stable when
a workspace is renamed, and still legible to a person reading the store's
repository list.

**6. Only an authentication refusal stops a create.** A `401` or `403` from
the pre-create credential check is a `400` with no workspace, secret or
repository written. Every other failure — DNS, a `5xx`, a timeout — answers
`201` with `status` `pending` and reaches the client as `failed` with
`clone_error`, because a forge that is briefly down is not a bad request.

**7. Parley accounts for the bytes it writes** and refuses an operation that
would take a repository past the 1 GB cap, naming that limit, because no
error code does. The 500 MB and 10,000-ref figures of decision D17 are
provisional: their fixtures are generated rather than committed, skip with a
named reason in the fast gate, and run in conformance against a real store,
where the real ceilings are measured and the figures corrected.

**8. The archive push is not forced.** Branches that fast-forward and
branches the remote does not have are pushed; a branch the remote refuses
leaves the repository in the store and the workspace is archived anyway. No
commit is lost and no history is rewritten on someone else's forge.

## Consequences

Parley owns protocol code — pkt-line framing, the capability advertisement,
`ls-refs` and `fetch` in v2, and `receive-pack` — and owns its correctness.
That code is exercised by every workspace rather than by a rarely-run path,
and 07's git server relays through the same client, so it is not written
twice.

Streaming the pack instead of materialising objects is what makes a copy of
this size conceivable inside a Worker. The same choice sets the grain of
recovery: a failed transfer resumes at a ref batch, never in the middle of a
pack, because git offers no way to resume one.

The capability probe runs before packages 02 to 04 are implemented, although
this package depends on them. It needs neither, and three later packages are
designed on its answer; finding out at package nine would invalidate them.
If a capability turns out to be missing, D16's fallback is still open and
09 chooses it.

The off-Cloudflare implementation has to hold a bare repository well enough
to receive a pack and serve one. That is more than a stub, but it is work
07 needs regardless, and it is what makes the contract suite a real
measurement rather than two implementations agreeing about nothing.
