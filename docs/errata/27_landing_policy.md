# Errata: 27_landing_policy

Where the implementation of package 27 (`.specs/27_landing_policy`)
differs from what the package says, or decides what it leaves open.

## The policy is composed in `internal/hub`, in two parts

The package wires its operation into `cmd/parleyd/main.go` and has
`store.go` register a schema step with `internal/store`. The hub is
composed in `internal/hub` (ADR 01), and `cmd/parleyd` is not touched.
`internal/policy/store.go` declares `policy.SchemaStep`
(`policy_revisions`), and the hub part adds it with `c.AddSchemaStep`, as
every other package does.

The part is split in two functions of `internal/hub/policy.go`.
`composePolicy` adds the schema step, the routes, the teardown remover and
the start hook that binds the component. It comes before `composeChanges`
in `parts`, because the change component's own start hook reconciles the
changes whose source moved while the hub was stopped. That records versions,
and each version asks the landing evaluator, which must be bound by then.
`composePolicyEvaluation` comes after `composeLand`. It registers the
component as the change component's landing evaluator and a `policyGate` as
the land component's policy gate. Both registrations are on the instances
while the hub is composed (ADR 08), so a test's `Compose` replaces them.
The change and land test worlds remove them to test their own component
alone.

## The structural cause is asked of the change component

The package reads `nothing_to_land`, `target_not_found` and `merge_commits`
from the change record. The record holds none of them: the change component
measures them from git each time, and they live only in `landing.reason`.
The code also has a fourth reason, `unrelated_history` (erratum 22). The
policy asks the change component for the cause through
`(*change.Component).LandingCause(ctx, q, ch)`. It answers `draft` for a
draft without measuring it. For any other change it answers the reason the
measurement of its head against its target branch gives, or nil. All four
reasons block the landing in the same way.

## The landing evaluator is asked for drafts and structurally blocked changes

This supersedes the rule of erratum 22 ("A draft still runs the stack
linker and the observers") and erratum 23 ("The landing is re-evaluated by
one helper") that the landing evaluator is not asked for a draft or for a
change with a structural cause.

The package lists the requirements of a draft and of a structurally blocked
change too. `change.LandingEvaluator.EvaluateLanding(ctx, tx, ch, v, cause)`
now takes the cause: `draft`, a structural reason, or nil. A new version
(`afterVersion`) and every re-evaluation of the change component
(`reevaluateLanding`) ask a registered evaluator in every case, draft and
structural cause included. The evaluator blocks the landing for the cause
and still lists the requirements. With no evaluator registered, the stored
landings are the same as before: ready with no requirements, blocked for
the cause with none, and a draft's landing is left as it is when a new
version is recorded.

The evaluator runs inside the change component's transaction. It reads the
policy, the workspace and the evidence through that transaction and does
not run the announcement step: the change component's callers run it.

## A re-evaluation stores the landing, then announces, in one transaction

`(*policy.Component).Reevaluate(ctx, workspace, number)` opens a store
transaction and calls `ReevaluateIn(ctx, tx, workspace, number)`, which is
exported for the callers that own a transaction (approvals, checks, a policy
replacement). In that transaction it does the following:

- reads the change; an unknown number or slug is `policy.ErrChangeNotFound`
  (the change component's `change_not_found`), and a landed or abandoned
  change is `policy.ErrChangeClosed`, with nothing written;
- reads the current revision of the policy of the change's target workspace,
  which is revision 0 for a task fork;
- reads the cause and the files, and evaluates with the installed evidence
  source;
- stores the result with `SetLanding`;
- runs `AnnounceLandable`, the one writer of `parley.change.ready`.

An error from either step rolls back both.

`SetLanding` also sets the change's `updated_at`. Each re-evaluation
therefore moves `updated_at`, even when the landing comes out unchanged. A
policy replacement re-evaluates every draft and open change of the
workspace, so it moves the `updated_at` of each one.

## The files of a change are those between its base and its head

The files are read with `DiffChanges(base_sha, head_sha)` in the repository
of the change's target workspace, which holds the head of every version, a
fork's included. Both paths of a rename or a copy are kept, as the change's
own `paths` keeps them, and the list has no cap. The base is the change's
current `base_sha`, which the stack linker sets to the head of the change
below (erratum 24). A version without a merge base is read from the empty
tree.

## The land gate judges the version the attempt lands

The gate is asked outside any transaction, about the version the attempt
lands (`GateInput.Version`, `Head`, `Base`). Since repair #189 that version
can be older than the change's current one. When it is the current version,
`policyGate` runs `Reevaluate`, which stores the landing, announces the
change and answers its status, reason, requirements and revision. For an
older version it calls `EvaluateVersion`, which evaluates the files
`Base..Head` with the evidence of that version against the current revision
and stores nothing. No structural cause is applied to an older version:
the change component measures the cause at the current head, which says
nothing about the older version's commits. A change that closed after
the land was asked is the gate's error, and the attempt fails. The adapter
lives in `internal/hub/policy.go`, so that `internal/policy` does not import
`internal/land`.

## Other decisions

- **The event's action is derived.** The package names action `update` on
  `parley.policy.update`. `event.Input` has no action: the event component
  derives `update` from the last segment of the type.
- **The user lookup is by selector.** A `user:` entry is looked up with
  `identity.UserBySelector`, which reads a UUID-shaped value as an id and a
  value containing `@` as an e-mail. Usernames come from the providers and
  have neither shape.
- **The policy routes have their own body limit.** `PUT …/policy`
  declares `MaxBodySize` 1 MiB, so that a document of 65 537 bytes reaches
  the validator, which names it, whatever `[server].max_body_size` is.
