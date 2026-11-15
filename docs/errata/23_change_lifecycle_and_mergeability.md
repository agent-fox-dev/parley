# Errata: 23_change_lifecycle_and_mergeability

Where the implementation of package 23
(`.specs/23_change_lifecycle_and_mergeability`) differs from what the package
says, or decides what it leaves open.

## The new state lives in schema steps of the change component

The package asks for schema migrations "registered with `internal/store`".
Each owning package declares its own `store.Step`, so the columns this
package adds are three steps of `internal/change`, added in
`internal/hub/change.go` right after `change.SchemaStep`:
`AutoLandSetterStep` (who turned `auto_land` on), `MergeableStep` (the pair
of commits a value was computed for, and the last computed value) and
`AnnouncedStep` (the head announced landable).

## The collaborators and entry points are methods of the change component

`AnnounceLandable`, `ForgetLandable`, `StoreMergeability`,
`ScheduleMergeable`, `ScheduleTargetMergeable`, `Abandon`, `Reconcile`,
`AutoLandSetter` and `RegisterRestacker` are methods of `*change.Component`,
as erratum 22 has every collaborator registered on the instance that calls
it. `SetAutoLand` stays in `open.go`, where package 22 put it, and now
stores the setter beside the flag.

## The landing is re-evaluated by one helper

Package 22 has no standalone landing re-evaluation: it is part of version
recording. The worker, the moves between draft and open, the retarget and
the restore re-evaluate through one unexported helper, `reevaluateLanding`:
a draft is blocked for `draft`; otherwise the structural reason the
measurement of the current head finds (`target_not_found`,
`nothing_to_land`, `unrelated_history`, `merge_commits`); otherwise the
registered landing evaluator; otherwise ready.

## A retarget writes the change and not its version

`SetBase` also rewrites the current version's record, and a retarget creates
no version and leaves the versions list as it was. The retarget therefore
writes the new base, stats and paths on the change alone. Open and retarget
share one target check (`checkTarget`), so their answers are the same; its
refusal of a change's own source branch now reads `target_branch: <b> is the
source branch itself` for both (open used to say `source.branch: <b> is the
target branch itself`; the type and status are unchanged).

## `mergeable` replays the commits the onto-commit lacks

`gitrepo.TrialRebase(onto, head)` replays `onto..head` and takes no base.
For a change whose base is an ancestor of the target's head, which is every
change whose target was not rewound, these are the current version's own
commits; for a change sitting on another change, they are its commits above
that change's head.

## Every trigger schedules inside its transaction

The store has no after-commit hook, and the ref-updated stage and the version
observer run inside the transaction that moves the refs. Every trigger marks
the change `unknown` and adds it to the worker's waiting set inside its
transaction. The check reads its pair of commits in a store transaction of
its own, which the store runs only after the triggering one has ended; a
check of a change that is no longer draft or open stores nothing. A target
move schedules every draft or open change into that branch, a change that
depends on another included, though its onto-commit is the lower change's
head; the extra check stores the same value.

An abandon drops the change's waiting check only after its transaction
committed (the abandon route, start-up reconciliation); an abandon inside
another component's transaction (a deleted source branch) leaves the
waiting check to the worker, which skips a closed change.

A restore that recreates its source branch writes the branch directly, not
through the ref writer, so it schedules the checks of the changes into that
branch itself.

## The intent rule judges the change as the edit leaves it

A `PATCH` whose result would be an open change with an empty intent while
`changes.require_intent` holds is refused, whatever member it edits: an open
change left without an intent by a later setting refuses an edit of its
labels until an intent is given. A `state` or `target_branch` equal to the
change's own is no edit and is dropped before this rule; on a landed or
abandoned change such a member still counts for the closed-change rules.
`depends_on` is not one of the editable members: on a closed change it is
ignored.

## Durations are whole units

`[changes].mergeable_debounce` and the `--timeout` and `--poll-interval`
flags of `parley change wait` take a whole number followed by `s`, `m`, `h`
or `d`, like every duration of the hub and the CLI. The checks' millisecond
values are set in the tests through the worker's dependencies and the CLI's
poll interval option, with `--timeout 1s`.

## Smaller decisions

- `409 change_exists` names the other change's ref (`demo:feat is already the
  source of demo#5`), for open and restore alike.
- `409 has_dependants` carries the numbers of the draft and open dependants
  as the member `dependants`.
- A draft moved to open is announced landable in the same transaction, after
  its `parley.change.open`; a change moved to draft forgets its announcement
  at once, whatever its `mergeable`.
- A restore deletes a branch it created when the restore then fails; a
  failure of the store commit itself, after the operation, may leave the
  branch, at the last version's head, for the next restore to find.
- A branch that cannot be recreated because another ref's name is in the
  way, or that is locked and still absent, is answered `409 conflict`.
- Start-up reconciliation logs and skips a draft or open change whose source
  workspace has no record: the delete of a workspace settles its changes.
- Every change event of the hub carries `number` in its metadata.
