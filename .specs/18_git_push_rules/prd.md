---
spec_id: "18"
spec_name: "git_push_rules"
title: "Accept or refuse pushes by rule"
status: "active"
created_at: "2026-11-10T18:31:13Z"
intent_hash: "9aad785b2b6686b4538a371265343f0ae245430db245cbdc91853bd5ff51af5c"
schema_version: 3
source: "docs/prd/05_git_server.md"
---

# Accept or refuse pushes by rule

## Intent

Every write to a ref of a workspace must pass the same ordered set of rules, whoever writes it and through whichever surface: refs the hub owns stay the hub's, protected branches move only the ways the owner allowed, a task fork cannot rewrite what it inherited, and a path-scoped token cannot add files outside its paths. Parley decides this in one rule engine, applies a push to all of its refs or to none, moves every ref by compare-and-swap, and records each accepted ref update as one event, so that agents can push freely and an owner can still trust what the branches hold.

## Goals

- One push rule engine and one ref writer in `internal/gitserver/pushrule` that the git server, the source control API (package 21) and landing (package 25, ref writer only) call with the same input, naming their surface as `via`; the engine is the part and that answers, for every ref update, allowed or refused with the rule's message, an `error_type` and, for a path refusal, the offending path.
- The rules are evaluated in the fixed order hub-owned refs, protected branches, default branch deletion, tracking branches, carry-patch push control, path-scoped tokens; the first rule that refuses decides; a rule that cannot be evaluated refuses with `internal error; try again`.
- A push over `git-receive-pack` is atomic across its refs: one refused ref means no ref moves, the pushed objects are discarded, no event is written and no change is touched.
- Every ref moves by compare-and-swap against the `before` value the writer sent; of two concurrent writers of one ref exactly one wins.
- The workspace owner or the admin token can fast-forward a protected branch directly while `protect.owner_push` is true; such an update is marked `direct`.
- One `parley.git.push` event per updated ref, in command order, written together, and still written after a hub stop between the ref update and the event.
- `[git].max_push_size` and `[git].timeout` bound a push, and every `git-receive-pack` request is counted in `parley_git_requests_total`.

## Non-goals

- The three git routes, Basic authentication, the order of checks before the exchange, the `401`/`403`/`404`/`409` answers, the receive-pack advertisement (GIT-FR-016), fetch, the `parley.git.fetch` event and `parley credential-helper` (GIT-FR-001 to GIT-FR-016, GIT-FR-070 to GIT-FR-073): package 17 `git_smart_http`. This package starts where that package has let a `POST …/git-receive-pack` request through.
- The `parley.*` push options and their validation, mapping an accepted push to a change or a version, and the `change …` and `warning: …` lines (GIT-FR-040 to GIT-FR-054): package 19 `git_push_to_change`. This package gives it two stages in the push pipeline (the options stage before the pack is received, and the mapping stage after the refs are written) and, for each update, the `direct` flag and the version a ref-updated stage recorded.
- Recording a version when a change's source branch moves, and abandoning a change whose source branch a push deleted (CHG-FR-010, CHG-FR-013): package 22 `change_opening_and_versions`. It registers a ref-updated stage with this package's ref writer, which runs it inside the commit step of every request.
- The source control API routes and how they turn a refusal into `403` with the `error_type`, a moved ref into `409 ref_moved`, and their own `parley.scm.commit` event (SCM-FR-020 to SCM-FR-028): package 21 `scm_write_api`. It calls the engine and the ref writer of this package.
- What carry-patch push control decides for the integration branch and for patch branches, forwarding a patch branch to origin, the mirror and the rebuild trigger (GIT-FR-031's table, CP-FR-077 to CP-FR-088): package 37 `carry_patch_push_control`. This package fixes only where that decision sits in the order, when the forward runs and what a failed forward does to the push.
- The path check when a version is created without new commits by other means (opening a change, restack) and at landing (LAND-FR-080 to LAND-FR-082): packages 22 `change_opening_and_versions` and 29 `auto_land_revert_and_path_limits`. This package asks a version-files lookup they provide.
- The glob matcher for token `paths` and its validation: package 06 `api_keys_and_access_tokens`. This package calls it.
- The keys, types, defaults and validation of the settings `protected_branches` and `protect.owner_push`: package 11 `workspace_read_update_settings`. This package defines what they do. How a branch glob matches is the one matcher of package 11 in `internal/workspace/branchglob.go`, which this package calls.
- Which branches of a task fork are tracking branches: package 15 `task_fork_creation`. This package asks. A fork's last-activity time: package 16 `task_fork_lifetime`, which registers the recorder this package's ref writer calls.
- Landing and sync from origin, which move protected branches without the engine (§8, §4): packages 25 `optimistic_landing` and 12 `workspace_sync_from_origin`. A land emits `parley.git.push` with `via: "land"` through this package's ref writer.
- The event log, visibility and the stream: package 52 `durable_event_log_and_stream`. The metrics endpoint: package 51 `audit_query_and_retention`.
- Any REST route and any CLI command. This package has none.

## Background

The repository holds no code yet; every file named here is new. The source is `docs/prd/05_git_server.md`: GIT-FR-020 to GIT-FR-035 (push processing and push rules), GIT-FR-060 to GIT-FR-062 (after a push), with §5.3 (ref update), §5.7 (quarantine), §5.8 (`parley.git.push`), §5.9 (limits) and §5.10 (edge cases). `docs/prd/08_landing_and_policy.md` §8.3.1 defines branch globs and the direct push; `docs/prd/06_source_control_api.md` SCM-FR-021 names the `error_type` of each rule; `docs/prd/14_sessions_audit_events.md` §14.3.6 requires the event of a ref update to survive a hub stop.

Package 17 serves `git-receive-pack` up to the point where the caller is authenticated, has write access and the workspace is `ready`. It then hands the request, the caller (credential kind, credential id, user, access level, binding, `paths`) and the workspace record to this package's push processor through the interface `PushProcessor` it declares in `internal/gitserver/push.go`; the processor returns `ok`, `refused` or `error`, and package 17 counts the request.

Code of this package:

| Path | Holds |
|---|---|
| `internal/gitserver/pushrule/engine.go` | the rule engine: input types, the order, the decision |
| `internal/gitserver/pushrule/rules.go` | rules 1, 2a and 3 |
| `internal/gitserver/pushrule/protected.go` | rule 2 and the owner exemption, over the branch-glob matcher of package 11 |
| `internal/gitserver/pushrule/carrypatch.go` | rule 4 as a hand-off to a registered controller, the forward phase |
| `internal/gitserver/pushrule/paths.go` | rule 5 |
| `internal/gitserver/pushrule/apply.go`, `journal.go`, `event.go` | the ref writer: forwards, compare-and-swap of all refs with the ref-updated stages in its commit step, `head_sha`, fork activity, the owed-event journal, `parley.git.push`, the post-push listeners |
| `internal/hub/gitserver.go` | wiring: the engine, the ref writer and their collaborators, the push processor handed to package 17, the journal reconciliation at start |
| `internal/gitrepo/quarantine.go`, `reftx.go`, `newcommits.go` | pack into quarantine, one transaction over several refs, new commits and their changed files |
| `internal/gitserver/receive.go`, `report.go` | the push processor behind `git-receive-pack` (the `PushProcessor` of package 17) and its status report |

Tests live beside each file. End-to-end tests with a stock `git` client are in `internal/gitserver/push_integration_test.go`.

## Requirements

### Ref updates and what the engine is asked

A ref update is `{ref, before, after}` (§5.3). `before` is the all-zero id for a create and `after` the all-zero id for a delete. An update is **forced** when it is neither a create nor a delete and `before` is not an ancestor of `after`. `<branch>` in a message is the ref name without `refs/heads/`.

The engine is asked with: the workspace (slug, kind, parent, default branch, mode, the effective settings `protected_branches` and `protect.owner_push`); the caller (access level `admin`, `owner` or `member`; credential kind `admin_token`, `api_key` or `pat`; the credential id and the user; whether the PAT is bound to a workspace; the PAT's `paths` or none); the ordered list of ref updates; and a view of the repository in which the new objects can be read. It answers one decision per update, in the same order: allowed, with the flags `created`, `deleted`, `forced` and `direct`; or refused, with a message and an `error_type`.

Evaluating writes nothing: no ref, no event, no outside effect. All updates of one request are evaluated before anything is written (GIT-FR-030).

### The rules, in order (GIT-FR-030)

For each update the rules run in this order, and the first that refuses decides the message:

| # | Rule | Refuses | Message | `error_type` |
|---|---|---|---|---|
| 1 | Hub-owned refs: `refs/parley/**`, for every credential including the admin token | create, update, delete | `refs/parley/ is maintained by the hub and cannot be pushed to` | `hub_owned_ref` |
| 2 | Protected branches | create, forced update and delete for every credential; fast-forward update unless the owner exemption applies | forced update or delete: `<branch> is protected and cannot be force-pushed or deleted`; otherwise `<branch> is protected; push another branch and open a change (git push -o parley.change)` | `protected_branch` |
| 2a | Default branch | delete, also when the branch is not protected | `<branch> is the default branch and cannot be deleted` | `default_branch` |
| 3 | Tracking branches, in a task fork | create, update, delete | `<branch> tracks the parent workspace and is read-only in this fork; push a new branch` | `tracking_branch` |
| 4 | Carry-patch push control | what the controller of package 37 refuses | the controller's message | `integration_branch` or `patch_branch` |
| 5 | Path-scoped tokens | the whole push | `path <path> is outside this token's allowed paths` | `path_not_allowed`, with the path |
| 6 | Everything else is allowed | nothing | | |

Rule 6 means that creates, fast-forward updates, forced updates and deletes of unprotected refs pass, in every namespace outside `refs/parley/`: branches, tags, notes and others (GIT-FR-026). Agents rewrite their own branches.

If a rule cannot be evaluated because something fails inside the hub (a setting cannot be read, an ancestry question to `git` fails, the tracking-branch lookup fails, the carry-patch controller reports a failure), the update is refused with `internal error; try again` and `error_type` `internal`. Rules never fail open (GIT-FR-033).

### Protected branches and the owner exemption (GIT-FR-034, §8.3.1)

A branch is protected when at least one glob of `protected_branches` matches its whole name (without `refs/heads/`), as the branch-glob matcher of package 11 decides (`*` any run of characters except `/`, `**` any run including `/`, case-sensitive). An empty list protects nothing. Rule 2 concerns `refs/heads/*` only.

A fast-forward update of a protected branch passes rule 2 when all four hold:

1. `protect.owner_push` is true (the default);
2. the caller's level on the workspace is `owner` or `admin`;
3. the credential is an API key, the admin token, or a PAT that is not bound to a workspace and has no `paths`;
4. the branch exists and `before` is an ancestor of `after`.

Such an update is a **direct push**; its decision carries `direct: true`. A create, a forced update and a delete of a protected branch are refused for every credential, including the admin token. An owner who pushes with a workspace-bound or path-scoped PAT is refused with the ordinary message, although the same person's API key would pass. With `protect.owner_push` false, rule 2 refuses every update for every credential. The exemption lifts rule 2 only: rules 1, 3 and 4 still apply to the ref.

A direct push is recorded (GIT-FR-035): its event carries `direct: true`, and the `direct` flag reaches the stage of package 19, which opens no change for that branch.

### Carry-patch hand-off (rule 4, GIT-FR-031, GIT-FR-061)

Package 37 registers a controller. For every update that passed rules 1 to 3 on a workspace whose mode is `carry_patch`, the engine asks the controller, which answers allow, refuse (message and `error_type`), or allow after a forward to origin. On a workspace in mode `standard` the controller is not asked. A `carry_patch` workspace with no controller registered is a rule that cannot be evaluated: `internal error; try again`.

Forwarding is the only rule with an outside effect. The ref writer runs the forwards last, in command order, only when every update of the request has passed every rule including rule 5, each with a 120 s limit. If a forward fails, the whole push is refused: that ref carries the controller's message (or `origin rejected push: timed out` when the limit passed), no ref of the hub moves, and branches already forwarded in the same push stay on origin.

After an accepted request, the ref writer tells the registered post-push listeners which refs moved: for a git push, for a write through the source control API (package 21) and for a land (package 25) alike. Each listener is called with the workspace slug, the accepted updates, `via` (`git`, `api` or `land`) and the session id of the request (null when there is none); package 37 registers one. The listeners run in the background; they never delay the answer and their failure changes nothing for the client.

### Path-scoped tokens (rule 5, GIT-FR-032)

Rule 5 runs when the caller's credential is a PAT with `paths`, after every update has passed rules 1 to 4. The checked commits are those reachable from the pushed heads and not reachable from any ref the repository had before the push; in a fork that includes the parent's refs. For each such commit the changed files are its differences against its first parent, or its whole tree for a root commit; a rename counts with both its old and its new path. A pushed annotated tag is checked through the commit it points to; notes and other refs are checked the same way.

If any changed path matches none of the token's globs (the matcher of package 06), the whole push is refused. The message names the first offending path: commits are taken oldest first (a parent before its children), and paths within a commit in byte order. The update from whose head that commit is reachable, first in command order, carries the message.

Deletes pass. An update that adds no new commit passes, unless the branch is the source of a `draft` or `open` change or the push opens one: then the files of the resulting version against its base are checked (LAND-FR-080), as the version-files lookup reports them. The lookup is asked with the slug, the branch, the new head and whether the update opens a change, and with nothing else: no target branch is passed, also for an update that opens a change, for which the lookup takes the target the open of package 22 defaults to. With no lookup registered there are no changes, and such an update passes. A force-push that rebases a branch onto a newer target does not trip on the target's commits: they are already in the repository. A credential without `paths` is never checked.

### Push processing over git (GIT-FR-020 to GIT-FR-027, §5.7)

The processor handles one `git-receive-pack` request in this order: read the commands and push options → run the options stage → receive the pack into quarantine → evaluate the rules for every ref → run the forwards → update all refs together, with the ref-updated stages inside the commit step → run the mapping stage → write the events → answer. The options stage and the mapping stage belong to package 19; without it the options stage accepts every option and the mapping stage adds no line. When the options stage refuses with a message M, the rest of the body is read and discarded, no object is stored, nothing is written, and the push is reported as `unpack ok`, `ng <ref> M` for every ref and one side-band line `error: M`.

- **Accepted.** HTTP `200`, `Content-Type: application/x-git-receive-pack-result`, a status report `unpack ok` and `ok <ref>` per command. The quarantined objects join the repository.
- **Refused by a rule or by the compare-and-swap.** A push is atomic across refs, whether or not the client asked for `--atomic` (GIT-FR-021). No ref is updated, the pushed objects are discarded and never become reachable or fetchable, no event is written, the mapping stage does not run. The refused refs carry their own message; every other ref is reported as `not updated: another ref in this push was refused`. Each refusal is reported twice (GIT-FR-023): as `ng <ref> <message>` in the status report, and as a side-band line `error: <ref>: <message>`. The HTTP status is `200`.
- **Pack failure.** A pack that cannot be read fails the push with `unpack invalid pack`; a pack larger than `[git].max_push_size` with `unpack pack exceeds the maximum push size`. Every ref is reported `ng <ref> unpacker error`. Nothing changes (GIT-FR-024).
- **No commands.** A request whose command list is empty is a successful no-op: HTTP `200`, no ref, no event, no mapping (GIT-FR-025).
- **Deletes only.** A push of deletes carries no pack and is processed the same way.
- **Default branch.** When an accepted push moves the default branch of a repository workspace, the workspace's `head_sha` holds the new id before the push is answered (GIT-FR-027).
- **Old clients.** A client that did not ask for `report-status` gets no report, and one without side-band gets no `error:` lines; the rules are enforced all the same.

### Compare-and-swap (GIT-FR-022)

Every ref update is a compare-and-swap against `before`. A ref that has moved, a create of an existing ref, and an update or delete of a missing ref are refused with `ref changed since it was read; fetch and try again`; the engine's callers see this outcome as `ref_moved`. All refs of one request are written in one transaction, so one mismatch leaves every ref where it was.

**A land is written without the engine.** The push rules do not apply to a land (§8). Package 25 builds an allowed verdict for its one update with `AllowedVerdict`, without calling `Evaluate`, and hands it to `Apply` with `via` `land`; the ref writer accepts it, so the protected-branch rule never refuses a land, while the compare-and-swap, the ref-updated stages, the journal, the event and the listeners work as for every request. Such an update has `direct: false`.

**Ref-updated stages (CHG-FR-010).** Other packages register stages with the ref writer. In the commit step the writer prepares the ref transaction (the compare-and-swap is then certain), opens one store transaction, runs the stages in registration order, commits the ref transaction and then the store transaction. A stage gets the store transaction, the workspace and caller facts (with the credential id), the session id, `via`, the version kind its caller named (`push`, `commit`, `ref`, `rebase` or none) and the accepted updates with their flags, and may report one version `{workspace_slug, number, version}` per update, which the writer returns with that update's result. This is how a version is recorded in the transaction of the ref update for every surface: package 22 registers the stage that records versions and abandons a change whose source branch was deleted. A stage that returns an error refuses the whole request before any ref moves: every update carries `internal error; try again`, and the writer hands the stage's error to its caller. If the hub stops, or the store commit fails, after the refs moved, the request stays accepted and the start-up reconciliation of package 23 records the missed version.

After an accepted request with `via` `git` or `api` on a task fork, the ref writer tells the fork activity recorder that package 16 registers, so that a push or an API write restarts the fork's early-removal clock.

A push never waits for another running operation and is never refused because of one. Two concurrent pushes to the same branch: one wins, the other is refused with the message above. A direct push that races with a land loses or wins the same way. Pushes to different refs do not interfere.

### The `parley.git.push` event (GIT-FR-060, §5.8)

One event per updated ref of an accepted request, in command order: `resource_type: "ref"`, `resource_id` the full ref name, `action: "push"`, `session_id` as the options stage set it (else null), the workspace slug (the fork's slug for a push to a fork), and `actor_type` `pat`, `api_key` or `admin_token` after the credential. Metadata: `ref`, `before`, `after` (40 hexadecimal characters; null on the side that does not exist), `created`, `deleted`, `forced`, `direct` (booleans) and `via`: `git` for a push, `api` when package 21 writes through the ref writer, `land` when package 25 does.

The events of one request are written together and have consecutive positions among the workspace's events. A refused push and a push with no commands write none.

Event problems never fail a push. Before the refs are written the ref writer records the request in a journal in the transactional store; the events and the removal of the journal entry are one transaction. If the event write fails, the client still gets its `ok`. At the next start, before it serves requests, the hub goes through the journal: for an entry whose refs hold their `after` values it writes the events; an entry whose refs were never written is dropped without an event.

### Limits, timeout and counting (GIT-FR-002, GIT-FR-062, §5.9)

- `[git].max_push_size` (default `0`, unlimited) is the largest accepted pack. A pack of exactly the limit is accepted; one byte more is the pack failure above. The REST body limit `[server].max_body_size` does not apply.
- `[git].timeout` (default `30m`) bounds one push. If it passes before the refs are written, the push is abandoned: nothing changes, the quarantine is discarded, and the report says `unpack push timed out`. Once the ref transaction has started the push is finished.
- A forward to origin has 120 s per branch.
- Every `git-receive-pack` request the processor handles is counted once in `parley_git_requests_total{service="receive-pack",result}`, by the handler of package 17 from the result the processor returns: `ok` for an accepted push and for a no-op; `refused` for a push refused by a rule, by the options stage or by the compare-and-swap; `error` for a pack failure, a timeout and `internal error; try again`.

### End-to-end flows

1. An agent with a PAT runs `git push` of a new branch to a workspace it owns, then rewrites the branch and force-pushes it. `parleyd` receives each pack into quarantine, evaluates the rules, moves the ref by compare-and-swap and writes one `parley.git.push` event per push; the second has `forced: true`.
2. A developer fast-forwards the protected default branch with an API key: accepted as a direct push, `head_sha` follows, the event has `direct: true`. The same developer then pushes the branch with a workspace-bound PAT, together with a second, unprotected branch: git prints the rule 2 message for the protected branch and `not updated: another ref in this push was refused` for the other; no ref moved and the new commits cannot be fetched.
3. An agent with a PAT limited to `docs/**` pushes a branch whose commit also changes `src/main.go`: the push is refused with `path src/main.go is outside this token's allowed paths`. The agent removes that file from the commit and pushes again: accepted.

## Design Decisions

1. **Profile: full.** Three end-to-end flows cross the git client, the processor, the engine, the repository and the event log.
2. **One engine, one ref writer, in `internal/gitserver/pushrule`.** The engine (`Evaluate`) evaluates; the ref writer (`Apply`, then its event step) applies an evaluated request (forwards, one ref transaction with the ref-updated stages, `head_sha`, fork activity, journal, events) and takes `via` and the version kind as parameters. The git server, package 21 and package 25 call the same two entry points, so the rules, the atomicity and the event cannot differ by surface. A sub-package keeps `internal/scm` from importing the whole git server.
3. **The engine takes plain facts, not components.** The caller's level, credential kind, binding and `paths`, and the workspace's facts, are passed in by the surface that already resolved them (package 17 for git). Lookups the engine cannot be given up front are narrow interfaces wired in `internal/hub/gitserver.go`: tracking branches (package 15), the carry-patch controller (package 37), the version-files lookup (packages 22 and 29), the event writer (package 52), the `head_sha` setter (package 10) and the ref-updated stages (packages 22 and 23). The fork activity recorder `ForkActivity` is not wired there: package 16 registers it from `internal/hub/fork.go`. Each has a default for a hub in which the owning package is not present: no tracking branches, no version files, and for the controller the fail-closed answer below.
4. **A `carry_patch` workspace without a registered controller is refused**, with `internal error; try again`. Allowing would be failing open (GIT-FR-033).
5. **"Owner" in the exemption is the access level `owner` of package 07.** On a task fork that is the fork's owner and the parent's owner. GIT-FR-034 says "the workspace's owner"; the access rule is the one definition of that.
6. **Rules 2, 2a and 3 apply to `refs/heads/*` only**, as their messages name a branch. Tags of the parent in a fork are not covered by rule 3 here.
7. **The hub parses the receive-pack request and writes the status report itself.** The pack goes to `git index-pack` with a quarantine object directory; refs are written with one `git update-ref --stdin` transaction carrying the expected old values, prepared before the quarantined objects are moved into the repository and committed after, so that a push that loses the compare-and-swap leaves no object behind. A ref held by another transaction at that moment counts as moved. `git receive-pack` is not run, because its report cannot carry the rules' messages for every ref and it has no place for the stages of GIT-FR-020.
8. **Rules are evaluated on the `before` the client sent; the compare-and-swap is checked when the refs are written.** A client that sends a false `before` to pass as a fast-forward is caught by the compare-and-swap.
9. **Which ref carries a rule 5 refusal.** The PRD says "the whole push". The message goes to the first update in command order from whose head the offending commit is reachable; the others get `not updated: another ref in this push was refused`.
10. **Commit order for rule 5** is oldest first, a parent before its children, as `git rev-list --reverse --topo-order` lists them; paths within a commit in byte order.
11. **Texts the PRD leaves open.** `unpack invalid pack`, `unpack pack exceeds the maximum push size`, `unpack push timed out`, the per-ref `unpacker error` (git's own wording), `origin rejected push: timed out` for a forward over 120 s, When the options stage of package 19 refuses a push, the stage's message M is the text: `ng <ref> M` per ref and `error: M` once, as package 19 specifies it (GIT-FR-051).
12. **A push with no commands answers `200` with an empty result body**: there is no ref to report on.
13. **Counting.** A compare-and-swap refusal counts as `refused` (the hub worked; the client must fetch). `internal error; try again`, a pack failure and a timeout count as `error`. The counter is package 17's: the processor returns the result through `PushProcessor` and package 17 increments, so no request is counted twice.
14. **Owed events are written at the next start only.** §14.3.6 requires exactly that; no retry runs in between.
15. **A fork's `head_sha` is not set by a push**: it is the parent's head of the fork's base branch (WS-FR-074).
16. **Versions join the ref update through stages (with packages 19, 21 and 22).** CHG-FR-010 asks for the version in the transaction of the ref update. A git ref cannot join a store transaction, so the writer prepares the ref transaction first, runs the stages in one store transaction and commits the two back to back; a stage failure therefore stops the refs from moving, and the one remaining gap (a stop between the two commits) is closed by start-up reconciliation. Package 19's mapping stage only opens changes and words the `remote:` lines; package 21 calls the same `Evaluate` and `Apply`.
17. **Branch globs are matched by package 11** (`internal/workspace/branchglob.go`). This package has no matcher of its own, so settings validation, the push rules, the source control API and landing cannot disagree on what a glob protects.
18. **Fork activity is reported by the ref writer**, once per accepted request on a fork, so that the git server and package 21 need no call of their own. The recorder is package 16's, reached through `ForkActivity`, which package 16 registers from `internal/hub/fork.go`.
19. **A land's verdict is built without `Evaluate` (with package 25).** `AllowedVerdict(updates)` in `internal/gitserver/pushrule` returns a verdict in which every update is allowed, with `created`, `deleted` and `forced` computed and `direct` false. `Apply` accepts it with `via` `land`. Why: landing has its own gates and moves protected targets; asking the engine would refuse them under rule 2.
20. **The post-push listeners are called by the ref writer, for every surface (with packages 21, 25 and 37).** The event step of the ref writer, which every caller of `Apply` runs after the ref transaction, ends by calling each registered listener on its own goroutine with the slug, the accepted updates, `via` and the session id. The git processor, package 21 and package 25 therefore send no notification of their own, and package 37 reaches API writes and lands through the one registration.
21. **The version-files lookup gets no target branch (with package 29).** Its arguments are the slug, the branch, the new head and the opens-change flag. Package 29 implements it and, for an update that opens a change, takes the target that package 22's open defaults to.
22. **`CompareAndSwapRef` and `IsAncestor` of `internal/gitrepo` are package 12's.** This package declares neither again: the repository view answers ancestry with `IsAncestor` (run over the quarantine's objects when a push has one), and the several-ref transaction in `reftx.go` reports a failed expectation with the `ErrRefMoved` of `CompareAndSwapRef`, which stays the writer of a single ref.
23. **Wiring is in `internal/hub/gitserver.go`**: process composition belongs to `internal/hub`.
24. **Project-wide:** Go; `git` 2.38 or later as a subprocess through `internal/gitrepo`, no git library; bare repositories; SQLite through `modernc.org/sqlite` behind `internal/store`; `net/http`; tests with `testing`, table-driven, `net/http/httptest`, repositories made with `git init` in `t.TempDir()`.

## Dependencies

- Package 17 `git_smart_http` (draft): the `git-receive-pack` route, the checks before the exchange, the `PushProcessor` interface with the caller and workspace it hands over, and the git request counter it keeps.
- Package 11 `workspace_read_update_settings` (draft): the branch-glob matcher in `internal/workspace/branchglob.go`.
- Package 12 `workspace_sync_from_origin` (draft): `IsAncestor` and `CompareAndSwapRef` with its `ErrRefMoved` in `internal/gitrepo`.
- Package 52 `durable_event_log_and_stream` (draft): writing events.
- Through it: package 10 `workspace_create_and_clone` (the workspace record, `internal/gitrepo`), package 07 `workspace_access_rule` (the level), package 06 `api_keys_and_access_tokens` (PATs, `paths`, the path glob matcher) and package 03 `server_lifecycle_and_configuration` (`[git]` configuration, the store, start-up).
- External: the `git` command, 2.38 or later.

## Glossary

- **Hub**: The `parleyd` server.
- **Workspace**: A repository the hub holds, identified by its `slug`. Either a registered repository or a task fork.
- **Task fork (fork)**: A workspace created from another workspace (its parent) for one task.
- **Tracking branch**: In a fork, a branch that existed in the parent at fork time. It is read-only in the fork and always equals the parent's branch.
- **Protected branch**: A branch that moves only by landing a change, by sync from origin, or by a direct fast-forward push from the workspace's owner or the admin token. Nobody can force-push or delete it. By default the default branch.
- **Direct push**: A fast-forward update of a protected branch by the workspace's owner or the admin token, over git or the source control API, without a change. Allowed while the workspace setting `protect.owner_push` is true.
- **Path-scoped token**: A PAT whose writes are limited to files matching a list of globs.
- **Bound token**: A PAT confined to one workspace (and the forks created with it).
- **Carry-patch workspace**: A workspace that tracks an upstream project and keeps an ordered list of patches on top of it.
- **Integration branch**: For a carry-patch workspace, the branch the hub rebuilds as upstream plus the ordered patches.
- **Hub event**: A record of something the hub did (`parley.*` types), with a sequence number.
- **Ref update**: One command of a push, `{ref, before, after}`; a create has an all-zero `before`, a delete an all-zero `after`.
- **Forced**: An update that is neither a create nor a delete and whose `before` is not an ancestor of `after`.
- **Quarantine**: Where the objects of a push are held until the push is accepted; a refused push's objects are discarded from it.
