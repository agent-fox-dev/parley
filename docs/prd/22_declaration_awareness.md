# Show which declarations a change touches and what they may break

## Intent

Agents that work in parallel learn not only that two changes touch the same
file but whether they touch the same declaration, and are warned of the
conflict that git cannot see: one change alters or removes a declaration
while other work, in flight or just landed, starts to use it. Both changes
rebase cleanly and the branch breaks. The hub reports this from what it
already holds, the versions of changes and the outline of their files,
before the land and without running a build.

## Goals

1. Every version of a change has the list of declarations it adds, removes
   and modifies, readable with one request.
2. An overlap entry says which declarations two changes have in common, so
   "same file, different functions" and "same function" can be told apart.
3. For every open change the hub lists its **reference risks**: each
   declaration whose signature it changes or which it removes, together
   with the lines that other in-flight changes, or commits landed since
   its base, added and that mention that declaration's name; and the same
   in the other direction.
4. The hub reports a check, `parley.references`, on every version: passed
   without risks, failed with them. A workspace whose policy does not name
   it lands exactly as today; one whose policy names it for some paths
   does not land a change with a risk on those paths without an override.
5. No push, commit or land waits for an analysis. The declarations and
   risks of a version that changes at most 50 files are readable within
   10 seconds of the push on a workspace with at most 100 open changes.
6. A workspace's owner can turn the feature off for the workspace, and a
   hub built without cgo provides it for Go files only.

## Non-goals

- Finding references by type, across packages or through a build. A
  reference here is a name match and is reported as one. A precise graph
  reported by external runners is a later PRD.
- Refusing a push or a commit. Awareness stays advisory (PRD §9).
- Claims on declarations. Claims stay on paths.
- Policy rules that select by declaration. Rules stay on paths.
- Predicting textual merge conflicts. `mergeable` (§7) answers that.
- Risks between changes of different target branches, or within one stack.
- Declarations of files over 1 MiB, binary files, and languages the outline
  does not support. Such files are counted and named, not analysed.
- Reading or replacing a declaration. It is PRD 21, and neither PRD needs
  the other.

## Background

Awareness (PRD §9, package 30, `internal/awareness`) keeps, per workspace
and target branch, which `draft` and `open` changes touch which files. Two
changes overlap when they have a file in common. `GET …/activity` and
`GET …/changes/:number/overlaps` return overlap entries
(`change`, `paths`, `count`), and `parley.change.overlap` announces a pair
once. §9.10 says what this cannot tell: "Two changes that edit different
functions of one file overlap."

Landing (PRD §8) is optimistic: a server-side rebase and a compare-and-swap.
Two changes that do not conflict textually both land. Checks (§8.3.6) are
reported by external runners on a version, "not on the rebased result of a
land", by design. So when change A changes the signature of `Greet` and
change B adds a call to `Greet` in another file, both are `clean`, both
pass their checks, both land, and the target no longer builds.

The hub can already see both halves. A version has its diff against its
base (§7). The outline (PRD 20, package 58) gives every declaration of a
file at any commit with its `signature` and line range, and agentkit-go's
`outline.CommentAndStringSpans` gives the comments and string literals of a
file, so a name inside one is not taken for a use.

## Requirements

### The declarations of a version

After a version is recorded (and after a retarget), the hub computes its
**touched declarations**, asynchronously, as overlaps are computed today
(AWR-FR-006).

- For each file the version changes against its base, in a language the
  outline supports, up to 1 MiB on both sides and not binary, the hub
  outlines the file at the base and at the head. A declaration is
  **touched** when a line the diff removes lies in its range at the base or
  a line the diff adds lies in its range at the head.
- A touched declaration is `{path, kind, name, container, signature, status, signature_changed}`
  with `status` one of `added` (in the head only), `removed` (in the base
  only) and `modified` (in both, matched by kind, name and container), and
  `signature_changed` true when it is `modified` and its `signature`
  differs. A rename therefore shows as one `removed` and one `added`.
- Changed lines outside every declaration are recorded per file as
  `outside_declarations: true`.
- At most 500 files are analysed for one version, in path order; the rest
  are counted in `files_skipped`. A file that could not be analysed is
  listed in `unanalysed` with its reason: `unsupported_language`,
  `too_large`, `binary`, `unparseable`, `limit`.

`GET /workspaces/:slug/changes/:number/declarations` (`changes:read`, the
access rule of CHG-FR-040; query `version`, default `latest`) answers
`{"change", "version", "state", "declarations": […], "files": [{"path", "outside_declarations"}], "unanalysed": [{"path", "reason"}], "files_skipped"}`
with `state` one of `pending` (not computed yet), `ready` and `off`.
`declarations` holds at most 2,000 entries, with `declarations_truncated`.
An unknown version is `404`.

### Overlaps by declaration

- An overlap entry gains `declarations`: the declarations both changes
  touch (same path, kind, name and container), at most 100, with
  `declarations_truncated`, and `declaration_count`. Both are present with
  `[]` and `0` when the two share files but no declaration, and `null`
  while either change's declarations are `pending` or `off`.
- `GET …/activity` takes an optional `declaration` (a name, exact) and then
  returns under `changes` only changes that touch a declaration of that
  name in a matching file.
- When a new version makes two changes share a declaration that shared none
  before, the hub emits one `parley.change.declaration_overlap` per change,
  naming the other, with `declarations` (at most 100) and `count`. The pair
  is announced once, under the rule of AWR-FR-005.
- `parley.change.overlap` and the definition of overlap are unchanged.

### Reference risks

A **reference risk** of a change C is a pair of:

- a declaration D that one side removes, or modifies with
  `signature_changed`, and
- one or more **mentions** of D on the other side: lines the other side
  adds, in a file of the same language as D's, that contain D's `name` as a
  whole identifier outside comments and string literals.

The other side of C is each of: every other `draft` or `open` change with
the same target branch that is not in C's stack; and the commits on the
target branch between C's base and the target's head. Both directions
count: C breaks what the other side starts to use (`direction: "outgoing"`),
and C starts to use what the other side breaks (`direction: "incoming"`).

- Names shorter than 3 characters are not matched. A mention in the lines
  that themselves make up D on the side that changed it is not a mention.
- The result is a name match and every risk says so: `match: "name"`.
- Risks are recomputed, asynchronously, after: a version or retarget of C,
  a version, retarget, abandon or restore of a change on the other side,
  and every move of the target branch.

`GET /workspaces/:slug/changes/:number/risks` (`changes:read`, CHG-FR-040)
answers `{"change", "version", "state", "computed_against": {"target_sha"}, "risks": [{"direction", "match", "declaration": {path, kind, name, container, status, signature_before, signature_after}, "with": {"change": {number, ref, title, session_id} | null, "landed_sha": string | null}, "mentions": [{"path", "line", "text"}]}]}`.
`state` is `pending`, `ready` or `off`. At most 100 risks and 20 mentions
per risk, `text` cut to 500 characters, with `risks_truncated` and
`mentions_truncated`.

When a change gains a risk it did not have, the hub emits
`parley.change.risk` for it, with the declaration, the direction and the
other side; a risk that persists across recomputations is announced once.

### Landing

The hub reports the check `parley.references` (§8.3.6) on the current
version of every `draft` and `open` change of a workspace with the setting
on:

- `pending` from the moment a version is recorded until its risks are
  computed; then `passed` when it has no risk and `failed` when it has at
  least one, with `summary` "N reference risks" and `details`
  `{"risks": N, "declarations": [up to 20 names]}`. `reported_by` is
  `{actor_type: "system"}`.
- It is reported again whenever the risks are recomputed and the status or
  the count changed. Each report emits `parley.change.check` and
  re-evaluates policy, as any report does (LAND-FR-044).
- It never carries over to a patch-equivalent version: the answer depends
  on the other side, not on the patch.
- A policy that does not name `parley.references` is unaffected:
  `landing.status` and every land are as today. A policy that names it in
  a rule's or the default's `checks` makes it a requirement like any other
  check, for the files that rule selects, and an override passes it as it
  passes any requirement (LAND-FR-002).
- A land is never held for a recomputation. Policy is evaluated with the
  check as last reported, so between a land on the target and the
  recomputation it causes, another change may land with a risk that was
  not reported yet. The check narrows the window; it does not close it.
- A land attempt gains `reference_risks`: for each change it lands, the
  number of risks last computed, or `null` when none was computed.
- Callers can no longer report a check whose name starts with `parley.`:
  `PUT …/checks/:name` answers `400`, `error_type: reserved_check_name`.
  Results reported under such a name before stay as they are.
- With the setting off the hub reports nothing, so a policy that requires
  `parley.references` there leaves its requirement `pending`.

### Turning it off

- A repository workspace gains the setting `declaration_awareness` (bool,
  default `true`); a fork shows its parent's value.
- With the setting off, both new routes answer `200` with `state: "off"`
  and empty lists, overlap entries carry `declarations: null`, and nothing
  is computed or kept.
- Turning it on computes the current version of every `draft` and `open`
  change.
- On a hub built without cgo, files in languages other than Go are
  `unanalysed` with `unsupported_language`.

### CLI

- `parley change declarations <slug> <number>` with `--version`.
- `parley change risks <slug> <number>`.
- `parley change checks` shows `parley.references` with the others;
  `parley change check <slug> <number> parley.<anything>` exits 1 with the
  `reserved_check_name` envelope.
- `parley activity` gains `--declaration <name>`.
- `parley change overlaps` prints the new fields as the route returns them.

### Metrics

`parley_declaration_analysis_duration_seconds`,
`parley_declaration_analysis_total` by outcome (`ok`, `partial`, `failed`),
`parley_reference_risks_total`.

### End-to-end flows

1. **Same file, different functions.** Two agents push changes that both
   edit `api/router.go`. `parley change overlaps demo 12` shows change 13
   with `paths: ["api/router.go"]` and `declaration_count: 0`; they carry
   on.
2. **A signature and a new caller.** Change 12 adds a parameter to `Greet`.
   Change 13, from another fork, adds `Greet(name)` in `cmd/hello.go`.
   After 13's push, `parley change risks demo 12` lists `Greet`, outgoing,
   with change 13 and the line in `cmd/hello.go`; 13 lists the same risk,
   incoming; both sessions receive `parley.change.risk`.
3. **The caller landed first.** Change 13 lands. Change 12's risk now names
   `landed_sha` instead of change 13. The agent of 12 rebases, fixes the
   caller, pushes; the new version has no risk.
4. **A policy that requires it.** The owner adds `parley.references` to
   the `checks` of the rule for `billing/**`. Change 14 touches
   `billing/charge.go` and has a risk: `parley change land demo 14`
   answers `409 policy_blocked` with the requirement and the failed check.
   A change with a risk outside `billing/**` lands as before.

## Design Decisions

1. **A reference is a name match, and says so.** It needs no build, works
   on every version within seconds, and errs towards reporting, as claims
   do (AWR-FR-002). Precision is a later PRD.
2. **Only removals and signature changes are the breaking side.** A body
   edit that changes behaviour is not something a parser can judge.
3. **Mentions are searched only in added lines**, not in the whole tree:
   existing callers are the changing agent's own job and its checks' job;
   what nobody can see is the caller that appears elsewhere at the same
   time.
4. **A new event for declaration overlap**, because the file overlap of a
   pair is announced once and the shared declaration may come later.
5. **Asynchronous, like overlaps.** A push returns before the analysis.
6. **Stacks are left out**, as in overlaps: the upper change is built on
   the lower one.
7. **On by default with a workspace setting**, as the code index is.
8. **The risk reaches landing as a hub-reported check** (owner's
   decision). Policy already decides by path which checks a change needs,
   so the owner chooses advisory or required per path, and awareness
   itself stays advisory.
9. **A land does not wait for the check to be recomputed.** Waiting would
   serialize landing, which §8.3.6 rules out for external checks for the
   same reason.
10. **The 10-second target** is set for the size of workspace Parley is
    sized for (§15); it is a target to measure, as `docs/performance.md`
    does for the others.
11. **A split is expected**: the declarations of a version and their route;
   overlaps by declaration; reference risks, their route and event;
   landing.

## Dependencies

- Package 30 `awareness` (active, all tasks done): overlap entries, the
  activity query and the overlap event this extends.
- Package 22 `change_opening_and_versions` (active, all tasks done):
  versions and their diffs.
- Package 58 `scm_file_outline` (active, all tasks done): declarations.
- Package 11 `workspace_read_update_settings` (active, all tasks done): the
  new setting.
- Packages 27 `landing_policy` and 28 `approvals_and_checks` (active, all
  tasks done): the check the hub reports, the reserved name, and the new
  field of a land attempt.
- ADR 09 (the job queue) and ADR 08 (the notice when a ref moves).

## Verified External API

`github.com/agent-fox-dev/agentkit-go` v0.10.0, read with `go doc`:

- `outline.Outline(ctx context.Context, abs string, src []byte, opts outline.Options) (outline.File, error)`
  and `outline.OutlineMany(ctx context.Context, srcs []outline.Source, opts outline.Options) ([]outline.File, error)`;
  `Decl` has `Kind`, `Name`, `Container`, `Signature`, `Exported`,
  `StartLine`, `EndLine`.
- `outline.CommentAndStringSpans(ctx context.Context, path string, src []byte) (spans [][2]int, ok bool)`:
  byte ranges of comments and string literals; `ok` is false without a
  grammar.
- `outline.LangFor(path string, src []byte) string`.

**Unverified**: that `CommentAndStringSpans` answers for Go on a hub built
without cgo (it is documented as tree-sitter's); if it does not, on such a
hub mentions inside Go comments and strings are reported too.

## Glossary

- **Declaration**: one entry of a file's outline (PRD 20).
- **Touched declaration**: a declaration a version adds, removes or
  modifies against its base.
- **Signature**: the outline's one-line form of a declaration, without its
  body.
- **Mention**: an added line that contains a declaration's name as a whole
  identifier, outside comments and string literals.
- **Reference risk**: a declaration one side removes or changes the
  signature of, with the mentions the other side adds.
- **Other side**: for a change, the other in-flight changes on its target
  and the commits landed there since its base.
