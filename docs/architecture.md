# Architecture

Parley ships as two programs: `parleyd`, the hub (server), and `parley`, the
command-line client. Both are written in Go, in the module
`github.com/agent-fox-dev/parley`, and the server uses the standard library's
`net/http` with no web framework.

## Package layout

| Package | Holds |
|---|---|
| `cmd/parley` | the `parley` program: it calls `cli.Run` and exits with its status |
| `cmd/parleyd` | the `parleyd` program: `main` calls `hub.Run` with the process's arguments, environment and working directory and exits with its status |
| `internal/access` | the workspace access rule, the one place that decides what a caller may do on a workspace; the minimal workspace record (slug, owner, org, kind, parent, status, the creating token of a fork) in the transactional store, the caller's level on a workspace, the decision for a declared need and the REST access check, the readable set for reads across workspaces, what the binding of a workspace-bound PAT covers, and the binding guard that confines such a PAT on every REST route and, for the git server, on git, and the minting, listing and revoking of bound tokens (`MintBound`, `POST` and `GET /workspaces/:slug/tokens`, `RevokeBoundTo`, and the maintenance task `token_binding_sweep`, `SweepBindings`) |
| `internal/awareness` | awareness of other work: the index of the files each draft and open change touches, kept from the event log by a background worker, the overlaps between changes and their `parley.change.overlap` events, the activity query, and claims with their expiry, links to changes and cleanup (see [Awareness](#awareness)) |
| `internal/buildinfo` | the version, commit and build time the binary was built with, shared by both programs |
| `internal/carrypatch` | carry-patch workspaces: the upstream credential, the upstream fetch and base, the set-up after a fresh clone and its repetition, the patch list in the transactional store, the guard and the routes under `/workspaces/:slug/patches`, the git runner every carry-patch operation uses, push control of the integration and patch branches (the push rules' controller, the forward to origin, the trigger after an accepted write with its mirror, and the landing guard), the rebuild trigger interface, the rebuild jobs' routes and enqueue, the rebuild handler that recomputes the integration branch, with its remembered-resolution entries, and the rollback of the integration branch and the rebuild preview (see [Carry-patch workspaces](#carry-patch-workspaces) and [Durable jobs](#durable-jobs)) |
| `internal/change` | the changes: the change record and its versions (`SchemaStep`), opening (`Open`, `CheckOpen`, `Authorize`), recording versions (`RecordVersion`, and `RefUpdated`, the ref writer's ref-updated stage), retention (`SourceDeleted`, `ForkDeleted`, `WorkspaceDeleted`), the lookups and setters other features call in their own store transaction, the collaborators registered on the instance, and the routes under `/workspaces/:slug/changes` (see [Changes](#changes)) |
| `internal/cli` | the `parley` client: its root command and its subcommands, built with `github.com/spf13/cobra`, each declared through one helper ([ADR 02](adr/02-declare-every-cli-command-through-one-helper.md)), the client configuration file, the resolution of the hub URL and the credential, the one HTTP client the commands send their requests with, the printing of a command's result on stdout and of its warnings on stderr, the shared wait for a long operation (`--wait`, `--timeout`, `--poll-interval`), and the error envelopes and exit statuses every command ends with |
| `internal/config` | the configuration loader: `config.toml` from `$XDG_CONFIG_HOME` or the working directory, `${NAME}` references, a registry of keys and whole sections that decides what the file may hold, and the keys it owns itself, among them the fork limits of `[forks]` (`Config.Forks`), whose `max_ttl` it refuses when shorter than `default_ttl`, the read bounds of `[scm]` (`Config.SCM{ReadTimeout, MaxFileSize}`), each refused at zero or below, the mergeability debounce of `[changes]` (`Config.Changes{MergeableDebounce}`), refused when it is not a duration, and the landing options of `[land]` (`Config.Land{MaxAttempts, Timeout, Workers, CommitterName, CommitterEmail}`), refused below 1, at a timeout of zero or below, or empty |
| `internal/event` | the hub events: every significant state change as one event, written in the caller's store transaction with a hub-wide `seq` that is never reused; the event record and its JSON form, the emit calls `Emit` and `EmitNow`, the log's bounds, the read by position and the expiry of old positions, the following of commits for waiters and `OnCommit` callbacks, the counters, the keys of `[events]`, the route `GET /api/v1/events/log` with its long-poll and the Server-Sent Events stream `GET /api/v1/events` with its limits ([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)) |
| `internal/forge` | the forge component, the hub's client of the GitHub or GitLab repository a workspace's remote lives on: the resolution of a remote target into repository, forge kind, API URL and credential (`Resolve`, `ParseRemote`, `ResolveCredential`), the outbound `Client` and its typed `Failure`s, the mapping of a failure to a hub error (`MapError`), the route helper every forge route is built with (`Component.Route`), forge paging, the two wire mappings, the read routes (see [Forge reads](#forge-reads)), the write routes with their events (see [Forge writes](#forge-writes)), and the webhook secret's routes, the public delivery ingress, the reduction of a delivery to hub events, the delivery record and its list (see [Forge webhooks](#forge-webhooks)) |
| `internal/forge/forgetest` | test support, imported by test code only: `New` and `NewPlain` serve a fake GitHub and GitLab API over TLS or plain HTTP, record every request, serve the entities a test sets (`SetObject`, `SetList`, `SetListIn`, paged by `per_page`/`page` with a `Link` `rel="next"`) and answer what a test scripts (`Script`, an answer that may be held until the test releases it and then passed on); below a repository's API path it keeps in-memory GitHub repositories and GitLab projects for the writes (issues, comments, labels, pull requests: `SetNext`, `AddIssue`, `Issue`, `AddLabel`, `SetLabels`, `Labels`) |
| `internal/forge/number` | the rule of a pull request or issue number (`Parse`), a leaf package of the standard library alone, shared by the hub's routes and the `parley` client |
| `internal/fork` | task forks: the lifetime rule `ResolveTTL(ttl, now, limits)`, the one judge of a requested `ttl` (a whole number above zero followed by `m`, `h` or `d`, from `5m` to `[forks].max_ttl`), which returns `expires_at` in whole seconds UTC or a `400 invalid_request` naming `ttl`, and `DefaultExpiry(now, limits)` for a fork created without one; the fork record (`Fork`: the workspace record plus the names of the parent's branches and tags at creation) and its store functions; and the `Component` with a fork's ref view (`Refs`), its computed head (`HeadSHA`, also shown as the `head_sha` of its workspace object) and the check of a write to one of its branches (`CheckBranchWrite`), and the parent-state check of a read or a write of a fork (`CheckParentState`); the handlers of `purpose` and `auto_change` in an update of a fork (`PurposeField`, `AutoChangeField`) and the guard of every update of a fork (`GuardUpdate`); and the routes `POST /workspaces/:slug/forks`, which creates a fork, and `GET /workspaces/:slug/forks`, which lists a workspace's forks in pages cut to the caller's readable set (`Routes`; see [Task forks](#task-forks)), with the collaborators a later feature registers on the component: the session-linking rule (`SessionLinker`), the sandbox component of the create (`SandboxCreator`) and the sandbox lifecycle of a removal (`SandboxLifecycle`); and the removal of a fork, the one place forks are removed: the fork remover of the workspace service's teardown set (`DeleteFork`, `RemoveForksOf`), the table of pending discards (`DiscardStep`) and the number of task forks (`CountTaskForks`); and the sweep that removes the forks whose lifetime has ended and repeats the pending discards (`RunPass`, `RunSweep`); and the early expiry of a fork whose work has landed, with its change reader (`ChangeReader`, `SourceChange`, `RegisterChangeReader`), the fork's last activity (`ActivityStep`) and the activity hook (`RecordActivity`) |
| `internal/gitrepo` | the hub's one way to run git, the `git` command as a subprocess with no git library; it imports nothing of the project. What a git process would cost many times over to read it reads from git's own files in process, and asks git whenever those files are not what it reads: `Open` checks the git directory's layout, `ResolveBranch` reads the branch's loose ref, else `packed-refs`, and `Branches`, `Tags`, `OriginHeads` and the fetches' listings of the refs below a prefix read the loose refs below it and `packed-refs` as `git for-each-ref` lists them (a symbolic or malformed ref, a name git would refuse, a linked worktree's git directory, the reftable backend and a directory that is no longer a git directory go to git), `Config` reads the key from the `config` file (a key with a subsection, an include, a continued line and a quoted, escaped or valueless value go to git), and `SetConfig` leaves a key that holds the value once already, `UnsetConfig` one that is not set, without running git. `IsAncestor`, `MergeBase`, `CommitsBetween`, `ReadCommits` and the listing of the commits a rebase or trial rebase replays read the commits from the loose objects and the packs of the repository and its alternates and walk their history as `git merge-base` does (a shallow repository, grafts, replace refs, an object it does not find, several merge bases, a range that holds a merge commit and a walk of more than 2048 commits go to git). `Git` (its zero value runs git from `PATH`; `Env` adds variables such as `GIT_SSL_CAINFO` to every git process, `Trace` sees the arguments of every invocation) runs each git process with a fixed minimal environment (`PATH`, `HOME` set to an empty private directory made, mode 0700, for that one process and removed when it returns, `GIT_CONFIG_NOSYSTEM=1`, `GIT_CONFIG_GLOBAL=/dev/null`, `GIT_TERMINAL_PROMPT=0`, `GIT_SSH_COMMAND=ssh -o BatchMode=yes`, so that the `ssh` of a `git@` origin fails rather than prompts, unless `Env` sets its own, `LC_ALL=C`), outside any repository, keeps git's standard error in the error it returns and, when the context ends, kills git with every process it started (its own process group) and waits for them. `ListRemote(ctx, url, cred)` returns origin's branches, tags and default branch as a `Remote`; `Clone(ctx, url, cred, dir)` makes a bare copy with every branch and tag and full history; both, like `FetchOrigin` below, run with `-c credential.helper=`. A `Credential` (user name and password; the zero value is none) reaches git only through the environment of that one process: `GIT_ASKPASS` names a script, written into that private directory, that prints `PARLEY_GIT_USERNAME` or `PARLEY_GIT_PASSWORD`, so the credential is in no URL, no argument and no configuration file of the copy. `FetchOrigin(ctx, dir, url, cred, deadline)` fetches every branch and tag of origin into the copy at `dir` in one `git fetch --atomic --force --no-tags` into a namespace of that one fetch, `refs/parley/fetch/<random>/`, which is read and removed, so that the tags' objects are in the copy and no extra ref outlives the fetch: after a failure right away, after a success in the one `update-ref --stdin` transaction that sets the origin heads `refs/remotes/origin/<name>` to origin's branches exactly (created, moved, removed for a branch origin no longer has); when no origin head changes, or git refuses that transaction because a new name is a directory of a removed one (`dev` replaced by `dev/next`), the namespace is removed alone first, then the origin heads are set in one transaction, or with the removals first; a fetch that fails, or whose namespace cannot be removed, changes no origin head; no branch or tag of the copy is written; it returns origin's branch heads and tags as `Fetched`, and after `deadline` kills git with every process it started and returns an error matching `ErrDeadline`. `Push(ctx, PushRequest)` pushes one commit of the copy at `PushRequest.Dir` to one branch of origin in one `git push --porcelain --no-follow-tags` of the single refspec `<commit>:refs/heads/<branch>`, with `-c credential.helper=` and the credential as above, so that no tag and no other branch is sent; `PushRequest.Force` adds the explicit lease `--force-with-lease=refs/heads/<branch>:<PushRequest.Lease>`, `Lease` being the commit origin's branch must still be at, or `""` for a branch origin must not have, never a plain force; the result is read from the status git reports for the ref, also when git exits with an error: `PushPushed` for a new branch or a fast-forward, `PushForced` for a forced push that replaced origin's branch, `PushUpToDate` when origin's branch is at the commit already (git reports it before it judges the lease), an error matching `ErrNonFastForward` for git's non-fast-forward and fetch-first refusals, one matching `ErrOriginMoved` for a forced push git refused because the lease is stale, and for every other failure a `*PushError` (`Branch`, `Class`, `Reason`, `OriginMessage`, `Err`) whose class `errors.Is` matches: `ErrOriginRejected` for a `[remote rejected]` status of the ref, its `Reason` origin's reason followed by the `remote:` lines its hooks wrote and its `OriginMessage` those lines alone (`""` when there are none); otherwise, when git reports no status for the ref, the class read from git's standard error, for the HTTPS transport and for the SSH transport's and forges' messages: `ErrUnauthenticated` (`Authentication failed`, `could not read Username`, HTTP 401, `Permission denied (publickey`), `ErrForbidden` (HTTP 403, `Permission to … denied`, `not allowed to push`), `ErrUnreachable` (name resolution, connection, proxy, TLS and certificate failures, `Host key verification failed`), judged before `ErrRepositoryNotFound` (HTTP 404, `not found`, `could not be found`) since such a text may hold a `not found` of its own, and `ErrPushFailed` for anything else, its `Reason` git's standard error without its boilerplate; a push whose context ended returns the error wrapping the context's error, git killed with every process it started. `(*Repo).Push(ctx, PushRequest)` is the same push run from the repository, whatever `PushRequest.Dir` says, by a git with the repository's environment, so that a quarantine's `View()` pushes a commit only the quarantine holds. `PushBranches(ctx, PushBranchesRequest)` offers several branches of the copy at `Dir` in one `git push --porcelain --no-follow-tags`, with `-c credential.helper=` and the credential as above, of one refspec `<commit>:refs/heads/<branch>` for each entry of `Heads` (branch to commit), so that no tag is sent, nothing is forced and nothing is deleted; origin takes or refuses each branch on its own, and it returns one `BranchPush` (`Branch`, `Commit`, `Result`, `Err`) for each branch git reported a status for, ordered by name: `Result` `PushPushed` or `PushUpToDate` for a branch origin took, `Err` an error matching `ErrNonFastForward`, or a `*PushError` of class `ErrOriginRejected` or `ErrPushFailed`, for one it refused; its error is nil once every branch has a status, a `*PushError` of the transport's class when git failed before any, and an error wrapping the context's error, with the statuses reported before, when the context ended and git was killed with every process it started. `Open(ctx, dir)` opens a copy as a `Repo` (an error for a file, an empty directory or nothing), whose methods never contact origin: `DefaultBranch` and `SetDefaultBranch`, `ResolveBranch`, `Branches`, `Tags`, `OriginURL`, `Config` and `SetConfig` (the copy's own configuration), the origin heads, kept as the refs `refs/remotes/origin/<name>`: `OriginHeads`, `OriginHead`, `SetOriginHead` and `DeleteOriginHead`, `CompareAndSwapRef(ctx, ref, new, expectedOld)`, which moves a ref only from `expectedOld` (`""`: creates it only while it does not exist) and otherwise, or when another writer holds the ref's lock, changes nothing and returns an error matching `ErrRefMoved`, `PrepareRefs(ctx, []RefUpdate{Ref, Old, New})`, the first phase of one `git update-ref --stdin` transaction over several refs (`Old` `""`: a create, `New` `""`: a delete), which checks every expected value and locks every ref without moving one and returns a `*RefTx` whose `Commit` moves them all and whose `Abort` none (the transaction outlives the context, since a killed git would leave its locks; one that deletes a packed ref holds the packed refs' lock until it ends, and another waits up to 10 s for that lock, `core.packedRefsTimeout`, before it fails with git's error), or, when an expectation fails (a moved ref, a create of an existing ref, an update or delete of a missing one, a ref another writer holds), a `*RefError` naming the first such ref and matching `ErrRefMoved` (`ErrRefNameConflict` for a name another ref is in the way of), `RefInTheWay(ctx, ref)`, the ref whose name keeps `ref` from being created, one being a directory of the other, `IsAncestor`, `NewCommits(ctx, tips, parentDir)`, the commits the tips reach that no ref of the repository nor of the repository at `parentDir` (a fork's parent, `""` for none) reaches, oldest first with every commit after its parents, `ChangedPaths(ctx, commit)`, the paths a commit changes against its first parent (its whole tree for a root commit, both paths of a rename) in byte order, and `ObjectPaths(ctx, id)`, the kind (`ObjectCommit`, `ObjectTree`, `ObjectBlob`) of the object an id reaches through any tags, with every path of a tree in byte order; a `Repo` opened by a `Git` whose `Env` names a quarantine in `GIT_ALTERNATE_OBJECT_DIRECTORIES` answers all of these over the quarantined objects too; `Missing(ctx, ids)` returns the ids that name no object (`git cat-file --batch-check`); the reads the source control API builds on, each of which takes a caller's string only once it has checked its form, refusing any other without running git, and hands git only ids and full ref names, through standard input or as `--opt=<value>`: `ListRefs(ctx, prefixes...)` lists, in name order, the refs below prefixes that end in `/` (every ref with none) as `Ref{Name, ID, Commit}`, `Commit` the commit `ID` reaches through annotated tags or `""`; `LookupRef(ctx, name)` is the `Ref` of exactly that full name, with whether it exists, and `ResolveRef(ctx, name)` the commit it reaches (a name `git check-ref-format` would refuse is not looked up: `ValidRefName(name)` is that one test, which every surface that names a ref to write checks it with); `ResolveCommit(ctx, hex)` is the one commit whose id begins with 4 to 40 hexadecimal characters, read with `git rev-parse --disambiguate`, which looks at objects only and never at refs, whether or not a ref reaches it; `PeelCommit(ctx, id)` is the commit a full id reaches through annotated tags; `ObjectInfo(ctx, name)` is the id, type and size (`Object`) of a full id or of `<id>:<path>`; their failures are the distinct errors `ErrNotFound`, `ErrAmbiguous` (an abbreviated id more than one commit's id begins with; objects of other types do not count) and `ErrNotCommit`; `ReadTree(ctx, commit, path, TreeRead{Recursive, After, Limit})` is the directory `path` (`""` the root) of a full commit id as `Tree{ID, Entries}`, each `TreeEntry{Path, Name, Mode, ID, Type, Size}` (`Size` a blob's alone), from one `git cat-file --batch-check` of `<id>:<path>` and its parent, sorted by path in byte order rather than git's tree order, the entries whose path comes after `After` (`""`: every one), the first `Limit` (0: every one): the directory's own from one `git ls-tree -z -l` of its tree id, or with `Recursive` every entry below it, directories included, read from the directories on the way down to `After`, the deepest first, each listed with `git ls-tree -z -l` and the contents of the entries after `After` from one streamed `git ls-tree -z -l -r -t <tree> -- :(literal)<name>…` that is stopped once the page is full, so that no directory before `After` and none past the page is read; a path that names nothing is `ErrNotFound`, one that names a file, symlink or submodule `ErrNotDirectory`; `LookupFile(ctx, commit, path)` is the `TreeEntry` of the file or symlink `path` of a full commit id (its mode, blob id, type and size), from one `git cat-file --batch-check` of the parent directory and one `git ls-tree -z -l` of its tree id, `ErrNotFound` for a path that names nothing and `ErrNotFile` for a directory or a submodule; `Blame(ctx, commit, path, first, last)` groups the lines `first` to `last` (`0`: the first and the last line) of the file `path` of a full commit id by the commit that last changed them, from `git --literal-pathspecs blame --porcelain [-L<first>,<last>] <commit> -- <path>`, which follows the renames of the file, into `[]BlameRange{StartLine, EndLine, Commit, OriginalPath, OriginalStartLine}` in line order, adjacent groups of one commit at adjacent original lines of one path merged (git names a group's path only when it first shows its commit, so a later group takes the path its commit was last shown with); a file without lines has no ranges; `Grep(ctx, commit, GrepQuery{Pattern, Regexp, IgnoreCase, AfterPath, AfterLine, MaxText}, each)` searches the files of the tree of a full commit id with `git grep -n -z -I --no-color -F|-E [-i] -e <pattern> <commit> --` (the pattern after `-e`, so never an option; binary files skipped) and calls `each` with each `GrepMatch{Path, Line, Text}` (the text without its `\n` or `\r\n`, with `MaxText` above 0 its first `MaxText` characters, the rest of the line not held) after the line `AfterLine` of the file `AfterPath` when it is set, git given the pathspecs of the files from `AfterPath` on (`:(literal)<AfterPath>`, then for each byte of it a wildcard of the paths that share the bytes before it and have a greater one there, and those it begins), none for a path git would not read literally or whose pathspecs pass 64 KiB, in path order, which for a tree is byte order of the whole path, then line order, killing git as soon as `each` returns `false`; with `Regexp` the escapes `\w`, `\W`, `\s` and `\S` outside bracket expressions are written as the POSIX classes they stand for, since macOS's C library does not know them; a pattern holding a newline matches nothing without running git, and an empty pattern, one holding a NUL byte, or a regular expression git refuses (exit 128, confirmed by searching the empty tree for it) is `ErrInvalidPattern`; `WriteBlob(ctx, id, w)` writes the bytes of a full blob id to `w` as `git cat-file blob` writes them, without holding them in memory; `History(ctx, from, HistoryQuery{FirstParent, Path, Follow, Since, Until, Author, Skip, Count})` is the commits reachable from a full id, newest first, read with `git --literal-pathspecs log` (so a path is never pathspec magic), the author matched with `--regexp-ignore-case --fixed-strings`, the committer times with `--since-as-filter` (plain `--since` stops at the first older commit) and `--until`, each given as `@<unix> +0000`, and `Skip` and `Count` counted by git except with `Follow`, whose renames git finds after it counted; `ReadCommits(ctx, ids)` reads full ids through `git cat-file --batch` into `Commit{ID, Parents, Tree, Author, Committer, Message}` (`Signature{Name, Email, Time}`, the time in UTC, the message as stored); `AuthorZone(ctx, id)` is the zone of the author time of one full commit id, read with `git rev-list --no-walk --date=raw`, for a commit made with that author; `CommitStats(ctx, commits)` is each commit's `DiffStats{FilesChanged, Insertions, Deletions}` against its first parent (the empty tree for a root commit) from one `git diff-tree --stdin --always --root -M -C --numstat`; `DiffChanges(ctx, from, to)` is the `FileChange{Path, OldPath, Status, Insertions, Deletions, Binary, OldID, NewID}` list between two full ids (`from` `""` the empty tree) from `git diff-tree -r -M -C --raw --numstat`, in byte order of path, `Status` one of `ChangeAdded`, `ChangeModified`, `ChangeDeleted`, `ChangeRenamed`, `ChangeCopied`, `ChangeTypeChanged`, a binary file `Binary` with no lines, a missing side's id `""` (every comparison finds renames and copies with the same `-M -C`, a copy when its source changed in the same comparison); since a change version's files are compared when it opens and again by the awareness worker and the policy check, the last 256 answers of up to 256 file changes each are kept in process, by repository, sides and `Env` (`diffcache.go`), and given again without git while the repository still holds both sides and git reads its history as stored (no shallow file, grafts or replace refs, and no `Env` that moves its objects); `Stats(changes)` adds them up; `MergeBase(ctx, a, b)` is the newest common ancestor of two full ids from `git merge-base`, `""` when they have none; `DiffPatches(ctx, from, to, PatchOptions{Context, Binary, Max}, each)` streams `git diff-tree -r -p -M -C -U<Context>` (`--binary` with `Binary`) and calls `each` with the index of each `DiffChanges` entry and its part of the patch, split at the lines beginning `diff --git ` (the deletion and creation git writes for a type change, under one header line, are one part), a part longer than `Max` bytes passed as `nil` and never held whole, and kills git as soon as `each` returns `false`; `AheadBehind(ctx, base, head)` counts the commits only `head` reaches and only `base` reaches (`git rev-list --left-right --count`), and `CommitsBetween(ctx, base, head, limit)` lists those only `head` reaches (every commit of `head` for `base` `""`), oldest first, every commit after its parents, the first `limit` when above 0; `TrialRebase(ctx, onto, head)` replays the commits of `head` not in `onto`, oldest first, listed with their trees and parents alone by `git rev-list --reverse --no-commit-header --format=%H %T %P` and never read whole, on top of `onto` in the object store alone, with no work tree, no index and no ref update (a commit whose parent's tree is what it is replayed onto is taken as it is, any other merged by `git merge-tree --write-tree -z --messages --merge-base=<parent> <ours> <commit>`, a parentless commit of the result written with `git hash-object -t commit -w` as the next `<ours>`, since `merge-tree` takes trees only from git 2.45, so the compare route needs git 2.40, the first with `--merge-base`), refuses with `ErrMergeCommit` before replaying when a merge commit is among them, may leave objects no ref reaches, and returns `nil` when every commit applies or, for the first that does not, the hub's one conflict report `*ConflictReport` (`conflictreport.go`: `TargetSHA`, `HeadSHA`, `BaseSHA` the merge base, `CommitSHA`, `Files []ConflictFile{Path, Kind, Base, Ours, Theirs, LandedBy}` with each side a `*ConflictSide{SHA, Mode, Path}` or `nil`, `LandedChanges` and `Change`, encoded with every member present: `[]` for an empty list, `null` for no `Change` and no merge base), one file per `CONFLICT` message of `merge-tree` (and per conflicted path no message names), its path ours' path where ours holds the file (a rename's new path in the target), its kind `submodule`, `binary`, `rename` or `mode` (`CONFLICT (distinct modes)`; the side git records aside as `<path>~<commit>` to keep both types gets its own path back) where git names one, else by the sides that hold the file: `add_add` (no base, different content), `modify_delete` (not in the head commit), `delete_modify` (not in ours), `mode` (two modes that do not merge and content that does), `content`; `ConflictText(ctx, file)` (`conflict.go`) is a `ConflictFile` merged as text by `git merge-file -p --no-diff3 -L target -L base -L change`, run outside any repository on the three blobs written to a temporary directory removed on every return (an empty file for a missing side), so that each region both sides changed is between `<<<<<<< target`, `=======` and `>>>>>>> change`, with no base section whatever the git configuration; `TrialRebaseFrom(ctx, base, head, onto)` is the trial of the commits `head` reaches and `base` does not (every commit of `head` for `base` `""`), as `RebaseFrom` replays them, its report's `BaseSHA` `base`; `Rebase(ctx, onto, head, at)` (`rebase.go`) is the same replay keeping its result, both built on one loop (`replay`), the trial writing no commit of its own: each replayed commit is written with `CreateCommit`, keeping the author (with the author's zone, read by `git rev-list --date=raw --format=%H %ad`, since `ReadCommits` gives UTC) and the message, with `HubSignature(at)` as committer; a commit whose replay leaves the tree it is replayed onto as it is is left out, and one whose parent already is what it is replayed onto is kept as it is; it returns the new head (`onto` when nothing is left), or no head and the same `*ConflictReport` as the trial, or `ErrMergeCommit` before writing anything, and like the trial uses no work tree, index, lock or ref; `RebaseFromResolving(ctx, base, head, onto, at, resolutions)` (`conflict.go`) is `RebaseFrom` with a `Resolution{Content, Delete}` for some paths, by path: a commit whose every conflicted path has one is replayed as `merge-tree`'s tree with each such path replaced by its content (mode: the change's, else the target's, else `100644`) or its file removed, at every commit where the path conflicts, and the first commit with a conflicted path that has none is the report, as `RebaseFrom` returns it; `RebaseFromAs(ctx, base, head, onto, committer)` (`replay.go`) is `RebaseFrom` with the committer `Signature` given instead of `HubSignature(at)` (`Rebase` and `RebaseFrom` are this one replay with `HubSignature(at)`), which also returns, for each commit of `base..head`, oldest first, a `ReplayedCommit{From, To}`: `To` is `From` for a commit kept as it is, the new commit for one replayed and `""` for one left out; a conflict or an error returns no head and no commits; `RefValues(ctx, refs)` maps each of the named refs that exists to the id it points at; `NewQuarantine()` makes a push's `Quarantine`, an empty object directory `objects/incoming-<random>` inside the repository's own: `Receive(ctx, pack)` stores a pack in it with `git index-pack --stdin --fix-thin` (`GIT_OBJECT_DIRECTORY` the quarantine, the repository's objects its alternate, so a thin pack is completed), `View()` is the repository opened with the quarantine as an alternate object directory, `Migrate()` moves its objects into the repository, each pack's index last, leaving a file the repository already has as it is and noting the files it adds, `Unmigrate()` removes those files again, each index first, and `Discard()` removes the quarantine with whatever it still holds, so that a refused push leaves nothing readable; `GIT_QUARANTINE_PATH` is never set, since git refuses ref updates under it; `CreateFork(ctx, parentDir, forkDir)` makes a fork's repository without copying history: an empty bare repository (`git init --bare`) whose `objects/info/alternates` names the parent's object directory by its absolute path, so that no object or pack is written into it, the parent is not changed, every object of the parent, also one it receives later, is readable through it, and what is pushed to it is stored in it alone; it is built in a temporary directory `.<name>.tmp-<random>` beside `forkDir` and renamed into place once complete, the temporary directory removed on an error (one a creation the process did not survive left is named so that `IsForkTemp(name)` reports it, for the workspace workers' start to remove), and refuses, with an error matching `fs.ErrExist`, a `forkDir` where something exists already, which it leaves as it is, and a `parentDir` that is not a repository; `Remove(dir)` removes a copy or a fork's repository; `UploadPack(ctx, ServeRequest)` serves a client of the hub's git server: it runs `git upload-pack --stateless-rpc` (with `--advertise-refs` for ref discovery) on `Dir` with the minimal environment, the settings of `Config` as `-c` options, `GIT_PROTOCOL=version=2` only when `Protocol2` is set, the request read from `Stdin` and the answer written to `Stdout` as git writes it, and returns a `*ServeError` holding git's standard error when git fails, or git's text `upload-pack: not our ref <id>`, git not run, for a fetch of either version that wants an object no ref of the repository reaches (each want's id read as git reads it, in either case and whatever follows it), an error wrapping `ErrRequestTooLarge`, git not run, for a version 2 request, or the wants of a version 0 one, over `ServeRequest.MaxRequest` bytes (zero: `DefaultMaxRequest`, 16 MiB), or an error wrapping the context's error once git and every process it started were killed at the end of the context; `ReceivePack(ctx, ServeRequest)` runs `git receive-pack --stateless-rpc` the same way, with `receive.advertisePushOptions` and `receive.advertiseAtomic` set and always under protocol version 0; `CopyRefs(ctx, src, dst, RefCopy{Names, Prefixes, Prune})` makes the named refs of the repository at `dst` what they are at `src`: each of `Names` set to `src`'s value or deleted when `src` lacks it, every ref of `src` below one of `Prefixes` set, every other ref of `dst` below `Prune` deleted; the deletions run first in one `update-ref --stdin` transaction, then the refs whose value differs are set in one forced local `git fetch --atomic --no-tags`, which transfers no object when `dst` borrows `src`'s objects; a ref already right is not written, and it returns what it set and deleted (`Copied`); two copies into one `dst` at once can fail on each other's ref locks, so the caller keeps them apart. `ApplyOperations(ctx, parent, ops)` (`treewrite.go`) applies an ordered list of `Operation{Op, Path, From, To, Content, Mode}` (`OpPut`, its mode `ModeFile` when none is given, `ModeExecutable` or `ModeSymlink`, whose content is the link's target; `OpDelete`; `OpMove`, keeping the blob and the mode; `OpChmod` to `ModeFile` or `ModeExecutable`) to the tree of a commit, each to the tree the one before it left, checked against the parent's files as `git ls-tree -r -z` lists them, and returns the id of the tree they leave, the parent's own when they change nothing; the first operation that does not apply is an `*OperationError{Index, Path, Err}`, `Index` zero-based, wrapping `ErrInvalidOperation`, `ErrNoFile` (a delete, move or chmod of a missing path or a directory), `ErrPathExists` (a move onto a file or a directory), `ErrUnderFile` (a parent directory is a file), `ErrIsDirectory` (a put onto a directory) or `ErrSubmodule` (on or below a `160000` entry), checks the applier makes itself because `git update-index` replaces a file by a directory, or a directory by a file, without refusing; the new contents are written by one `git hash-object -w --no-filters --stdin-paths`, and the tree is built in a temporary index file of the call's own (`GIT_INDEX_FILE` in a private temporary directory removed on every exit, never the repository's index or a work tree) by `git read-tree`, one `git update-index -z --index-info`, removals first, and `git write-tree`. `CreateCommit(ctx, CommitSpec{Tree, Parents, Message, Author, Committer})` writes a commit with `git hash-object -t commit -w --stdin` from those parts alone, both signatures with their own times and offsets and nothing from the environment or a git configuration, `<`, `>` and newline left out of names and addresses and a newline ending the message, after checking that the tree is a tree and each parent a commit (a name with nothing else or a zero time is an error); it moves no ref. `ValidPath(path)` is the one test of a path an operation names. `HubSignature(at)` is the hub's identity, `HubName` `parley` and `HubEmail` `parley@localhost`, the committer of every commit the hub makes. `Revert(ctx, onto, commits, as)` (`revert.go`) undoes each of `commits`, in the order given, one commit each on top of `onto`, by a three-way merge in the object store alone (`merge-tree` with the undone commit as merge base, the current head as ours and the commit's parent as theirs), each with git's revert message (`Revert "<subject>"`, `This reverts commit <id>.`) and `as` as author and committer, leaving out an undo that changes nothing; it returns the last commit, or the `ConflictReport` of the first undo that does not apply, and moves no ref. For a second remote, `FetchRemote` fetches every branch of a URL into `refs/remotes/<name>/*` (pruned, no tags, nothing written to the configuration) and `FetchOriginBranch` one branch of origin into `refs/remotes/origin/<branch>` (`ErrNoSuchBranch` when origin lacks it); `Repo.UnsetConfig` removes a configuration value, `Repo.FetchBundle(ctx, path, refs)` fetches a git bundle file and sets the given refs, each at a tip of the bundle, in one ref transaction (the bundle's own refs go below a namespace of the fetch's own, removed after), `Repo.CreateBundle(ctx, path, refs)` writes a bundle of refs with their whole history (both for the checkpoints of sandboxes), and `SanitizeGitText` cleans git's text of URL user information (`***@`) and given secret values. The scratch working tree of a rebuild (`AddScratch`, `Scratch.Pick`, `Merge`, `Learn`, `Reset`, `Remove`, `DiscardScratches`, with git rerere per command), `ConflictFiles` and the rebuild preview's `TrialMerge(ctx, ours, theirs)` (`merge.go`: a `TrialMergeResult{Base, Tree, Conflicts}` of a merge in the object store alone, a conflict a result and any other failure an error) are described under [Durable jobs](#durable-jobs) |
| `internal/gitrepo/gitrepotest` | test support, imported by test code only, and the repository's one test origin: `NewOrigin(t, opts...)` makes an empty bare repository whose default branch is `main` and serves it over TLS with `net/http/httptest` and git's file-based HTTP transport (`git update-server-info` after every change), or with git's smart transport through `git http-backend` under `WithSmartHTTP()` (git cannot read a repository without refs over the file-based one, so an empty origin needs it); `SmartOrigin(t, opts...)` is `NewOrigin` with the smart transport and `http.receivepack` enabled, so that it takes pushes; `WithBasicAuth(user, password)` makes it answer `401` without them, `WithRepoPath(path)` serves it at `/<path>` (such as `acme/widgets.git`) in place of `/origin.git`, and `Accept(user, password)` changes the credential it takes while it serves; `Commit(branch, msg)`, `Branch(name, ref)`, `Tag(name, ref)` and `SetDefaultBranch(branch)` change it, `History()` builds the tests' usual history on it (`main` with three commits, `dev` branching off its second with one more, the tag `v1` on its first), `Head(branch)` reads it; `URL()` is its https URL and `Env()` (`GIT_SSL_CAINFO=<CAFile()>`) makes git trust it; `Requests()` counts the requests it received, `Block()` holds every later request until `Release()`, `Refuse(status)` answers every request with a status, `Reject(message)` installs a `pre-receive` hook that refuses every update writing `message` (`""` removes it), `RejectRef(ref, message)` one that refuses only a push that updates `ref`, and `StallRef(ref)` one that holds every push of `ref` until `Reject("")` or `Close()` and then refuses it, `ServeUntrusted(true)` makes it serve, on every later connection, a self-signed certificate that `CAFile()` does not vouch for, and `Close()` shuts it down; `Git(t, dir, args...)` runs git on a repository directory, such as a copy the code under test made, away from the user's git configuration, and returns its trimmed output, `Run(dir, stdin, args...)` does the same with `stdin` as git's input and returns git's failure as an error instead of failing the test, and `Refs(t, dir, prefix)` maps the refs below `prefix` of such a directory, by their names without it (`""`: every ref, by its full name), to their object ids, and `SharedPrefix(t, dir)` writes commits to such a repository, on `refs/keep/prefix`, until two commits' ids share their first four characters, and returns them, and `CompareStream()` is the fast-import stream of the branches the compare and trial-rebase tests use (a clean head, `long` with `LongCommits` commits, one head per conflict kind, a merge), `BlameStream()` the one of the files the blame tests blame (runs of lines of three commits, lines of one commit whose original lines are apart, an empty file, a file renamed with a line added), and `SearchStream()` the one of the files the search tests search (Go functions, a literal with regular-expression characters, a binary file, `long.txt`'s one line `SearchLong` of 2,000 characters, 450 lines of `hit`, a CR LF line, a path with a space) |
| `internal/gitserver` | the git server: the three smart HTTP routes under `/git` (`PatternInfoRefs`, `PatternUploadPack`, `PatternReceivePack`), which `Component.Mount` registers outside the REST API, the fixed order of checks every git request passes (authentication by the HTTP Basic password, the rate limit, the binding guard, the scope of a PAT, the resolution of the repository with the access rule, the validation of the request, the state of the workspace and of a fork's parent, the opening of the repository, and for a task fork the copy of the refs it shows of its parent's), the one plain-text refusal of each, and the exchange with git of a request that passed them: ref discovery and the fetch exchange are served by `git upload-pack --stateless-rpc` through a `Runner` (`gitrepo.Git` is one), under protocol version 2 only when the `Git-Protocol` header has the entry `version=2`, the answer streamed and flushed as git writes it, a gzip body decompressed, an empty or invalid body and a run longer than the timeout answered in band with an `ERR` packet, every run configured for partial clone with the filters `blob:none`, `blob:limit` and `tree` and for a want of any commit a ref reaches (pkt-line framing and the scanner that sees pack data and error packets go by in `pktline.go`); ref discovery for a push is served by `git receive-pack --stateless-rpc --advertise-refs`, always under protocol version 0, and a push exchange is handed, its body unread, to the `PushProcessor` (`push.go`), the one hand-off the push rules implement: it gets the response writer, the request, the `PushCaller` (credential type `pat`, `api_key` or `admin_token`, credential id, user, level, the guard's `Binding`, and for a PAT its principal with the paths) and the workspace record, writes the whole answer and returns the `Result` (`ok`, `refused`, `error`) the request is counted with; without one a push is answered 500 internal error; the hub's is the `Receiver` (`receive.go`), which parses the commands and push options itself, asks the `OptionsStage` registered with `RegisterOptionsStage` (the hub's is `PushOptionsStage`, `pushoptions.go`, which judges the options with `ValidatePushOptions`, the pure `ParsePushOptions` and a parley.session's check by the `SessionLinker` registered on it, refuses an invalid one before the pack is read, and hands on the session and whether the push opens a change), receives the pack into a `gitrepo.Quarantine`, has `pushrule.Engine.Evaluate` decide and `pushrule.Writer.Apply` write every ref of the push or none, asks the `MappingStage` registered with `RegisterMappingStage` of an accepted push (the hub's is `ChangeMapper`, `changemap.go`, which maps each pushed branch through a `ChangeService` to a new version, an opened change or a warning, and whose `Outcome` lines and refusal reasons are worded in `pushreport.go`), has `pushrule.Writer.Finish` write its events and tell the post-push listeners, and answers with the status report (`report.go`, see Git server below); every request on the three routes is counted once in the counter `RequestsMetric` (`parley_git_requests_total`) by `RequestLabels` (service `LabelInfoRefs`, `LabelUploadPack` or `LabelReceivePack`, and the `Result`), of which `Component.Requests` returns a snapshot (`metrics.go`); a served fetch that sent a pack from a workspace whose setting `audit_fetches` is true is recorded as an `EventFetch` (`parley.git.fetch`) event through the `EventSink` (`event.go`); `Register` and `TimeoutFrom` own `[git].timeout` (`DefaultTimeout`, 30m), which bounds one request, a fork's ref copy and the run of git, or a push up to its ref transaction, together, and `Register` and `MaxPushSizeFrom` own `[git].max_push_size` (bytes, 0 for no limit), the largest pack the `Receiver` reads; `HEAD` on ref discovery gets the server's `405` envelope; `Component` is bound with `Deps` (`Credentials`, `Limiter`, `Guard`, `Access`, `Records`, `Parents`, `Repositories`, `ForkView`, `Copier`, `Runner`, `Push`, `Events`, `Settings`, `Timeout`, `Service`, `Logger`), where `ForkView` names a fork's tracking refs and the changes whose source it is (`ForkRefs`) and `Copier` (`gitrepo.Git` is one) copies them from the parent's repository into the fork's, where `Service`, the interface that serves a request once every check has passed it, replaces the component's own exchange when set; `StoreRecords` reads an organization and a workspace record from the store, `DiskRepositories` opens a workspace's repository with `gitrepo` at the place a function names |
| `internal/gitserver/pushrule` | the push rule engine, which decides every ref update of a write to a workspace by one ordered set of rules, whatever the surface: `Engine.Evaluate` takes a `Request` of plain facts (the `Workspace`: slug, kind, parent, default branch, mode; the `Caller`: level, credential type `admin_token`, `api_key` or `pat`, credential id, user, bound, paths; the `Update`s `{Ref, Before, After, OpensChange}` in request order, `ZeroID` for a create's before and a delete's after; a `View` of the repository that answers `IsAncestor`, `NewCommits`, `ChangedPaths` and `ObjectPaths`, which `*gitrepo.Repo` satisfies; and `ParentRepo`, a fork's parent's git directory) and answers a `Verdict` with one `Decision` per update, in request order: allowed with `Created`, `Deleted`, `Forced` (neither a create nor a delete, and before not an ancestor of after) and `Direct`, or refused with the `Message` and `ErrorType` of the first rule that refuses (`Verdict.Refused` reports any refusal); it writes nothing while it evaluates. The rules run in the order hub-owned refs (any ref under `refs/parley/`, for every caller: `hub_owned_ref`), protected branches (a branch under `refs/heads/` that a glob of the workspace's effective `protected_branches` matches, by `Protected(branch, globs)`, which asks the branch-glob matcher of `internal/workspace` for each glob: a forced update or a delete is refused for every caller with `<branch> is protected and cannot be force-pushed or deleted`, a create for every caller and a fast-forward outside the owner exemption with `<branch> is protected; push another branch and open a change (git push -o parley.change)`, both `protected_branch`; the exemption, while `protect.owner_push` is true, is a caller of level `owner` or `admin` writing with an API key, the admin token or a PAT neither bound nor carrying paths, and lets the fast-forward pass this rule only, as an update the decision marks `Direct`), default branch deletion (the delete of `refs/heads/<default branch>`: `default_branch`), tracking branches (on a fork, a branch the `Tracking` lookup names, created, updated or deleted: `tracking_branch`; the lookup is not asked for a repository and a nil one names none), carry-patch push control (on a workspace in mode `carry_patch`, the one `Controller` registered with `Engine.RegisterController` is asked once per update that passed the earlier rules and answers allow, refuse with its message and `integration_branch` or `patch_branch`, or allow after a forward to origin, which the decision carries as `Forward` and evaluation never runs; without a controller, or when it fails, the update is refused as `internal`), path-scoped tokens (for a PAT with paths only, and once every update of the request passed the earlier rules: every commit the request adds, reachable from an after value and from no ref of the repository nor of a fork's parent, is compared with its first parent, oldest first, and the first changed path, in byte order, that `identity.MatchAny` finds outside the token's globs refuses the first update in command order that reaches the commit with `path <path> is outside this token's allowed paths`, `path_not_allowed` and the path; an update that adds no commit is held by what its after value reaches through any tags: a commit on a branch the same way to the files the `VersionFiles` lookup reports for the version it makes, allowed when it reports none (a commit on any other ref makes no version and passes), a tree to every path in it, and a blob, which has no path to check, is refused with the ref's name as the path; deletes pass, and any other caller is not checked and its commits not read), the first refusal deciding; an update no rule refuses is allowed in every namespace outside `refs/parley/`, and an update whose settings, ancestry, tracking, new-commits, changed-paths, object or version-files question fails is refused with `internal error; try again` (`internal`). `Engine.Bind` hands it its lookups (`EngineDeps`: the `SettingsReader` of a workspace's effective `protected_branches` and `protect.owner_push`, read once per request and only for a branch update, a nil one giving the settings' defaults, which protect the default branch and allow direct pushes; `Tracking`; and `VersionFiles`, asked with the slug, the branch, the after value and the opens-change flag, a nil one reporting no version). The ref writer, `Writer.Apply`, is the one way the surfaces move refs: it takes an `ApplyRequest` (the `Verdict`, the `*gitrepo.Repo`, opened over a push's quarantine when it has one, the workspace and caller facts, the session id, `Via` `git`, `api` or `land`, the version kind `push`, `commit`, `ref`, `rebase` or none, an optional `BeforeCommit` hook, an optional `AfterAbort` hook, which runs when the request stops with no ref moved after `BeforeCommit` ran, and `Events`, the events the request owes beside its push events) and returns a `Result` with an `Outcome` (`accepted`, `refused`, `ref_moved`, `internal`) and one `UpdateResult` per update, its decision and the `Version` `{WorkspaceSlug, Number, Version}` a stage recorded for it. A verdict with a refused update writes nothing and runs no forward, and every allowed update is answered `not updated: another ref in this push was refused`; otherwise the decisions' pending `Forward`s to origin run first, in command order, each given the request's `*gitrepo.Repo` (so a forward reads a push's quarantined objects) and a context that ends after `WriterDeps.ForwardLimit` (zero means `DefaultForwardLimit`, 120 s): the first that returns an error, or has not returned when its limit passes, refuses the request (outcome `refused`) with the error's text, or `origin rejected push: timed out`, and error type `patch_branch` on its update and the not-updated message on the others, before any ref is written, runs no later forward and leaves the earlier ones as origin has them (a request whose own context ends meanwhile is refused as `internal`); then every ref moves in one `PrepareRefs` transaction against its before value: a ref not at it refuses the request as `ref_moved` (`ref changed since it was read; fetch and try again`, error type `ref_moved`, a name in the way of another ref's counting the same), then `BeforeCommit` runs, then the stages registered with `RegisterRefUpdatedStage` (`RefUpdatedStage`, given the store transaction and a `StageInput`) run in order in one store transaction (`WriterDeps.Transact`), at whose end the refs move; a hook or stage error aborts the refs, rolls the stages back, runs `AfterAbort`, refuses every update as `internal` and is returned unchanged, and a store commit that fails after the refs moved is logged with the request still accepted. After an accepted request the `HeadSetter` records the new head of a repository's moved default branch, and the `ForkActivity` recorder registered with `RegisterForkActivity` is told of a write through `git` or `api` to a fork; their failures are logged. `Apply` refuses a `Via` other than `git`, `api` and `land` with an error before anything is done. With an event log bound (`WriterDeps.Events`, an `EventWriter`, which `*event.Component` is), a request is entered in the push journal (`journal.go`: the table `push_journal` of `JournalStep`, one `JournalEntry` per request with the workspace and a fork's parent, the actor type and id, the credential id, the session id, `Via`, the `JournalUpdate`s with their flags and the request's own `Events`, in the column `JournalEventsStep` adds, written by `InsertJournalEntry` and read by `JournalEntries`, the metadata of an event read back being a `json.RawMessage`) in a store transaction of its own, after the forwards and before `PrepareRefs`, marked `Committed` in the stages' store transaction, the one the refs move in, and removed when the refs do not move. The event step, `Writer.Finish(ctx, result)`, which the caller runs once it is done with an accepted result (the git processor after its mapping stage), writes in one store transaction one `EventPush` (`parley.git.push`) event per update, in command order, with `resource_type` `ref`, the ref as `resource_id`, the credential type as `actor_type`, the user as `actor_id` (none for the admin token), the session id and the metadata `{ref, before, after, created, deleted, forced, direct, via}` (`before` null for a create, `after` for a delete), then the request's own `Events` in their order, and removes the journal entry; a failure of that transaction is logged, the entry kept and the request still accepted. It then calls each `PostPushListener` registered with `RegisterPostPushListener` on a goroutine of its own with a `PostPush` (the slug, the accepted updates in command order, `Via`, the session id), recovering a panic and logging an error, for every surface alike; a refused result writes and tells nothing. `Writer.Reconcile`, run as the hub starts, writes the events (its push events, then its own) of every journal entry marked committed, whatever its refs hold now, and of every other entry whose refs all hold their after values (absent for a delete), removing the entry in the same transaction, and removes without events an entry whose refs do not; an entry whose repository cannot be read or whose transaction fails is logged and kept for the next start; it opens the repositories with `WriterDeps.Repos` and reads the refs with `(*gitrepo.Repo).RefValues`. No lock spans a request. `AllowedVerdict(ctx, view, updates)` allows every update without the rules, with `Created`, `Deleted` and `Forced` computed as `Evaluate` computes them, for landing |
| `internal/hub` | the composition of the `parleyd` process: argument parsing, the scrubbing logger, the fixed start order, the bootstrap of the admin token, the HTTP server's options, the idempotency records in the transactional store, signals and the stop, and the `backup` command; the one place later packages plug their parts in ([ADR 01](adr/01-compose-the-hub-in-one-package.md)) |
| `internal/maintenance` | the maintenance loop: a registry of named tasks, and `Loop`, which runs one pass at once and one at each tick of an hourly timer, the tasks of a pass one after the other in a fixed order, and logs a failing task without stopping the others |
| `internal/hubtest` | test support, imported by test code only: `Start` bootstraps a hub with an admin token and serves it in-process on a free loopback port, `StartWith` does the same with more in `config.toml` (`Config.Server` inside `[server]`, `Config.TOML` after it), files in the config dir (`Config.Files`), variables added to the hub's environment (`Config.Env`) and a function run on the composition (`Config.Compose`), `(*Hub).Restart` serves the hub again on the same data directory and port, which stays bound while the hub is down, and `RestartEnv` does so with other added variables, `(*Hub).IssuePAT` stores a PAT, `CreateOrg` and `AddMember` make an org and a membership through the admin routes and `AddWorkspace` stores a workspace record, `Serve` runs a hub with given options until the test ends, `(*Served).Call` sends a request with a bearer credential and a body and returns the answer's status and body, `CallWith` does so with added request headers and `Exchange` also returns the answer's headers, `LogBuffer` holds a running hub's log for the test to read, `JobClock` (`NewJobClock`) is a job queue clock whose timers fire only when the test calls `Advance` and whose `Armed(d)` says a timer of `d` was asked for, and `WaitClosed` waits up to 10 s for a channel to close |
| `internal/identity` | who may use the hub: the admin settings in the transactional store (the bootstrap e-mail and the admin token's hash), the generation and hashing of admin tokens, and `VerifyAdminToken`, which compares a presented token's hash with the stored one in constant time; the identity records (users, orgs, memberships, API keys, PATs), the generation of API keys and PATs, and `Service`, the pipeline's authenticator |
| `internal/jobs` | the durable job queue: jobs of a type and workspace with an opaque payload and result, stored in the transactional store (`SchemaStep`), enqueued with one pending job per type and workspace (`Enqueue`, `EnqueueFollowUp`, `DuplicateError`), claimed by a dispatcher (`Run`, `Pass`) that runs at most 4 attempts at once and one per type and workspace on the handler registered for the type (`Handler`, `Register`), settled as `completed`, `failed` with backoff or `dead_letter` (`Permanent`, `WithDetail`), recovered at start (`Recover`), stopped with a 30-second grace, counted (`Depth`), cancelled, requeued and removed with their workspace (see [Durable jobs](#durable-jobs)) |
| `internal/land` | the landing: the land attempt record (`Attempt`, `SchemaStep`), one row per request to land a change or a stack, created running and finished once in a final status, the engine that lands an attempt by rebase onto the target head and records it with the move of the target, trying again on a target that moved, through the land journal (`JournalStep`, `Reconcile`) that completes or drops at the next start a land a stop interrupted (`engine.go`, `replay.go`, `commit.go`), the run again at a start of the attempts a stop left running (`recover.go`, `Recover`), the pool that runs at most `[land].workers` attempts at once, each under the deadline `[land].timeout` (`pool.go`, `RunPool`), the policy gate and override rule registration points (`gate.go`), the request guards, target guards, after-landing hooks and final observers (`hooks.go`), the follow-up of a recorded land: the delete of its source branches, the mergeability checks of the other changes into its target and the after-landing hooks (`after.go`), the land route, which admits its caller (`authz.go`), creates the running attempt (`Start`, also for a land no request asks for) and waits for it, the session linker registration point, the read routes of the attempts per change, per workspace and by id, and the reads of a change's current conflict report and of a `content` file of it as text with conflict markers (`current_conflict.go`, `conflict_handlers.go`); and the rules around landing (see [Land rules](#land-rules)): the path limit of a path-scoped token (`pathlimit.go`: `CheckPaths`, the version-files lookup `VersionFiles()` of the push rule engine, and `PathGuard()`, the restack's version guard and a request guard), the override (`override.go`: `OverrideGuard()`, a request guard, and `PendingOverride`, the override rule), the automatic lander (`autoland.go`: `AutoLander`, `AutoLandStep`) and the revert route (`revert.go`) (see [Landing](#landing)) |
| `internal/policy` | the landing policy, what each part of a workspace's repository needs before a change touching it may land: the policy document and its normal form (`Document`, `Rule`, `Require`), `Validate`, which checks a document's JSON text against the format and refuses it with an `*InvalidPolicyError` naming the first violation (`APIError` answers it as `400 invalid_policy` with `details` `{rule, index, field}`), the narrow user lookup it asks (`Users`), `MatchPath`, the matcher of a rule's `paths` and `exclude` globs (`*` any run without `/`, `**` any run, `?` one character other than `/`, every other character itself, on the whole path; distinct from `identity.MatchPath`, in which `?` is literal), `Select`, which names the rule governing one file on a change into one target branch (the first rule in document order whose `targets` match the branch through `workspace.MatchAnyBranchGlob`, absent `targets` matching every branch, whose `paths` match the file and whose `exclude` does not, else `default`), `NeverSet`, the document of revision 0 that a workspace without a policy and every task fork has, `Evaluate`, which turns a change into its landing (`Result{status, reason, requirements}`: one `Requirement{rule, paths, approvals{required, counted, approvers}, checks[{name, status}], block, satisfied}` per selected rule in document order, `default` last, `paths` in ascending byte order cut to the first 50; `blocked` with reason `draft` for a draft, else with the change's structural cause, else `blocked` when a requirement blocks, `pending` when one is unsatisfied, `ready` otherwise; `Result.Landing` converts it to `change.Landing`) from the change's facts (`ChangeFacts`, its structural cause included), its files (`ChangeFiles`) and the evidence source (`Evidence`, asked once per requirement through the caller's `Querier`; a failure of either read fails the evaluation), `Preview`, the same over a list of files with no evidence and no cause; both select the rules of one evaluation through one selector, which decides the rules' `targets` once and compiles their globs once, and stop with the context's error when it ends, looked at before each file; `MatchPath` matches one element of a glob against 64 positions of the path at a time and stops when no position is left, `EvidenceSlot`, the installed evidence source, `NoEvidence` (nothing counted, every check pending) until `RegisterEvidence` replaces it, the verdicts and check results on a change's versions (`ReviewStep`, the tables `change_approvals` and `change_checks`), the evidence source over them (`Facts`), the version observer that carries verdicts to a patch-equivalent version (`VersionObserver`), and the include expanders of the change (`ExpandApprovals`, `ExpandChecks`), the revisions of a workspace's policy (`Revision`, `Writer`, `SchemaStep`, `CurrentRevision`, `RevisionAt`, `InsertRevision`, `Replace`, which writes the next revision under an optional expected revision, `ErrRevisionMismatch` otherwise, and keeps the 50 highest), and `Component`, bound with `Deps` (the store, the workspace records, `WorkspaceReader`, the user lookup, `Users`, the event writer, `EventWriter`, the change records, `Changes`, which the change component is, and the repository opener, `Repos`), which serves `GET` and `PUT /workspaces/:slug/policy` and `POST /workspaces/:slug/policy/evaluate`, the dry evaluation of paths or a change that writes nothing, and the five routes of verdicts and check results (`Routes`), deletes a deleted workspace's revisions, verdicts and check results (`WorkspaceDeleted`), reads the files of a change (`Files`, its `ChangeFiles`: those between the change's `base_sha` and `head_sha` in its target workspace's repository, both paths of a rename or copy, no cap), is the change component's landing evaluator (`EvaluateLanding`), re-evaluates and stores a change's landing (`Reevaluate`, in a transaction of its own, and `ReevaluateIn`, in the caller's; `ErrChangeNotFound`, `ErrChangeClosed`), and every draft and open change of the workspace in the transaction of a `PUT`, and judges the version a land lands (`JudgeVersion`: the change read and the version judged in one transaction, the landing stored for the current version only); see [Landing policy](#landing-policy) |
| `internal/sandbox` | the hub's side of the sandbox feature: `Register` makes it the owner of the whole `[sandbox]` section of the configuration, and `FromConfig` checks that section after the load, with the config dir and `[server].external_url` in view, and returns its `Settings` with every default filled in, or one error listing every failure; the `Settings` give the profiles and the configured credentials by name (`Profile`, `DefaultProfile`, `Credential`) and in name order (`Profiles`, `Credentials`), as copies, so that they do not change after the load; `RunStartCheck` checks the runtime once at start (see [The start-up check](#the-start-up-check)), `ReleaseRange` and `SupportedReleases` hold and compare the supported OpenShell releases, `Service` is the feature of one hub (settings, runtime client, connected release) and declares the sandbox routes (see [Sandbox route declarations](#sandbox-route-declarations)), `ProfilesRoute` is the route `GET /sandbox-profiles`, `SandboxesRoute` the route `GET /sandboxes`, which lists for the admin token the sandboxes of the `Source` a later package registers with `(*Service).RegisterSource`, `RuntimeError` is the `502 sandbox_runtime_error` answer, `Covers` and `RequireCovers` are the no-widening decision over a sandbox's `HubAccess`, and `Recorder` is the metrics recorder of the sandbox packages; the stored sandboxes (see [Stored sandboxes](#stored-sandboxes)): the record (`Record`, table `sandboxes` created by `SchemaStep`, `NewID`, `WorkDir` `/workspace`) and its store functions (`Insert`, `Get`, `GetIn`, `ListByState`, `Update` with the `Set…` fields, `Remove`), `(*Service).Transition` and `TransitionTx`, the one conditional move of a sandbox's state with its event, the object (`Object`, `ObjectOf`), the delete and purge hooks (`RegisterDeleteHook`, `RegisterPurgeHook`, `RunDeleteHooks`, `RunPurgeHooks`), the create (`Create`, `CreateRoute`, `ParseTTL`, `ExpiresAt`, `CheckQuota` with the `Limits`, `FamilyRoot`, `Resolve`, the session linker `RegisterSessionLinker`), the hub credential (`MintHubCredential`, `RevokeHubCredential`, `HubCredential`), the scrubbing of sandbox text (`Scrub`, `SetValues`, `ValuesOf`, `ScrubSandbox`, `ForgetValues`), the provisioner (`RunProvisioner`, `WakeProvisioner`, `ResumeProvisioning`, `ProtectedRules`, `RegisterWorkspaceRuleProvider`, `RegisterPolicyComposer`, `RegisterRestorer`), the metrics (`ReportStates`, `CreatesMetric`, `SandboxesMetric`, `SecondsMetric`), the reads (`GetRoute`, `ListRoute`, `StoredSource`) and the fork component (`CreateSandbox`), bound once at start by `Bind(Deps)`; and the routes that use a sandbox (see [Using a sandbox](#using-a-sandbox)): `UseRoutes` (exec, file transfer, logs, attach behind one gate), the stored log lines (`LogLine`, table `sandbox_logs` created by `LogSchemaStep`, `LogLines` with `LogSelection`, `LogSourceProcess`, `LogSourcePolicy`, `MaxLogLine`, `PurgeLogLines`), their collector and retention (`CollectLogs`, `LogCollectInterval`, `LogLinesStored`, `RetainLogs`, `LogFollowInterval`), and the operation observers (`RegisterObserver`, `Observer`, `Operation`, the `Operation…` kinds and `ExecResult…` results, `ExecMetric`); and the rest of a sandbox's life (see [The life of a sandbox](#the-life-of-a-sandbox)): `Checkpoint` (`CheckpointState`, `CheckpointRef`, `CheckpointBound`, `CheckpointFileLimit`, `CheckpointsMetric`), `Stop`, `StartSandbox`, `Restore`, `Delete`, `Extend`, the idle and expiry passes and their loop (`IdlePass`, `ExpiryPass`, `RunLifecycle`), the reconciliation (`Reconcile`, `ReconcileAtStart`, `RunReconcile`), `CleanupRecords`, the workspace and fork collaborators (`RemoveSandboxes`, `WorkspaceArchived`, `RemoveWorkspaceRecords`, `DeleteSandboxes`, `HasLiveSandbox`) and the routes `CheckpointRoute`, `StopRoute`, `StartRoute`, `DeleteRoute` and `ExtendRoute`; the network policy of a sandbox (see [The network policy of sandboxes](#the-network-policy-of-sandboxes)): the workspace sandbox rules (`SandboxRule`, `RuleSchemaStep`), the rules approved for a sandbox (`ApprovedRule`, `ApprovedRuleSchemaStep`), the reported denials (`DenialSchemaStep`) and the proposals (`Proposal`, `ProposalSchemaStep`), the policy composer and rule provider `RegisterNetworkPolicy` registers, the policy collector (`PolicyCollector`, `CollectPolicy`), the routes `ProposalsRoute`, `ApproveRoute`, `RejectRoute`, `RulesRoute` and `RuleDeleteRoute`, and the clean-up `WithdrawProposals`, `PurgeNetworkPolicy` and `RemoveWorkspaceRules` |
| `internal/sandbox/runtime` | the hub's one boundary to the sandbox runtime: the `Client` interface with every operation the sandbox packages call, the plain data it exchanges, the four error kinds (`ErrNotFound`, `ErrInvalid`, `ErrUnsupported`, `ErrUnavailable`), the reachability flag (`Reachability`) and the metrics recorder (`Recorder`, `NopRecorder`) it reports to; it imports nothing of the project |
| `internal/sandbox/runtime/openshell` | the runtime client over the OpenShell Go SDK (`github.com/ai-openshell/sdk-go`, package `openshell/v1`), the only production package that imports the SDK (`openshelltest` and tests import it too): `BuildConfig` turns the checked `[sandbox]` section into the SDK's configuration, `New` builds the client of a section and `NewWithClient` the client over a given SDK client, such as the SDK's in-memory one |
| `internal/sandbox/runtime/openshell/openshelltest` | test support, imported by test code only: `Gateway`, which stands in for the OpenShell gateway: the SDK's in-memory client with provider profiles kept per workspace and a switch that makes it unreachable and reachable again |
| `internal/sandbox/runtime/runtimetest` | test support, imported by test code only: `Fake`, the in-memory runtime and the only test double of the runtime, with its test controls, and `RunContract`, the contract tests every implementation of the runtime client passes |
| `internal/scm` | the source control API: the write gate every write route is built with (`writeRoute`, see [Source control writes](#source-control-writes)), the shared ref update step every write moves a ref through (`refupdate.go`: the push rule engine and the ref writer, reached as the interfaces `Rules` and `RefWriter` in `Deps`), the refs write routes (`refs_write.go`: `POST /refs`, `PATCH` and `DELETE /refs/*name`), the commit route (`commit.go`: `POST /commits`, one commit from file operations through `gitrepo.ApplyOperations` and `CreateCommit`, its own body limit `DefaultMaxCommitSize`, 32 MiB, which `LimitCommitBody` replaces, and the commits' times from `Deps.Now`), the rebase route (`rebase.go`: `POST /rebase`, the branch replayed onto a revision through `gitrepo.Rebase`), the interface `ChangeLinker` (`linker.go`; `FindOpen`, `CheckOpening`, `Open`, registered with `(*Component).RegisterChangeLinker`), the interface `SessionLinker` (`session.go`; `Check`, registered with `(*Component).RegisterSessionLinker`, a refusal matching `scm.ErrUnknownSession`), the `parley.scm.commit` event (`scm.EventCommit`) handed to the ref writer as `ApplyRequest.Events`, and the read half: the `GET` routes under `/workspaces/:slug/scm` (`refs`, `refs/*name`, `commits`, `commits/:sha`, `tree`, `file`, `diff`, `compare`, `blame` and `search`), served by an `scm.Component` (zero value, `Bind(scm.Deps)`, `Routes()`), with the state check, the request recorder and the lookups every read shares (see [Source control reads](#source-control-reads)); and the one revision resolver every read (and every later write that takes a revision) goes through: `NewResolver(repo, view, defaultBranch)` over a workspace's repository (the interface `Repository`, which `*gitrepo.Repo` satisfies) and its ref view (the interface `RefView`, whose `Refs` lists `Ref{Name, SHA, Commit, Tracking}` and is read at most once per request; `RepoRefs(repo)` is the view of a workspace that is what its repository holds, every ref of it), `Resolve(ctx, rev)` and `ResolveOptional(ctx, rev)` (an empty `rev` is the head of the default branch), returning `Resolved{SHA, Full}`, `Full` set when the caller wrote the full 40-character id; a `rev` starting with `refs/` is that full ref name only, otherwise the branch or the tag of that name (both existing is ambiguous), otherwise 4 to 40 hexadecimal characters are a commit id; every failure to name exactly one commit, an expression such as `main~2`, `HEAD` or `--all` included, is the API error `ref_not_found` (404); `cache.go` gives every successful read its `ETag` and `Cache-Control` and answers `304` to a matching `If-None-Match` before the read's costly part; `commitObjects` builds the commit object of every route that answers commits, the change (`CommitChange{WorkspaceSlug, Number, Version}`) and session from the lookups and `stats` only where asked, and `params.go` holds the strict query readers (`true`/`false` booleans, RFC 3339 times, and `pathParam`, the one check of a repository path every route that takes `path` makes) the routes share; `tree.go` maps git's entries to the tree route's four types by mode (`120000` symlink, `160000` commit, a tree, any other blob); `file.go` serves a file raw (one byte range of `Range` honoured) or as JSON, refusing one over `Deps.MaxFileSize` (`DefaultMaxFileSize`, 32 MiB, when zero) before reading it; the handler wrapper bounds every read by `Deps.ReadTimeout` (`DefaultReadTimeout`, 30 s, when zero), answering `503 scm_timeout` or cutting the answer off; `diff.go` builds the file change object of every route that answers file changes (`FileChanges`, the commit route's `files=true` and the diff route) and serves the diff route: the merge base or `base` against `head`, then `ComputeDiff`, the diff computation the change diff of `internal/change` also calls: the `path` globs applied with `identity.MatchAny`, at most `MaxDiffFiles` (5,000) files and `MaxPatchBytes` (8 MiB) of patch text read through `DiffPatches` file by file, the patch form answered by `WritePatchText`; `compare.go` serves the compare route: `AheadBehind`, the first 200 commits of `CommitsBetween` as commit objects without `stats`, the first 5,000 file changes from the merge base to `head`, and `TrialRebase`, whose `gitrepo.ErrMergeCommit` is `400 merge_commits_unsupported` and whose `*gitrepo.ConflictReport` is the answer's `conflict`; `blame.go` serves the blame route: the file looked up with `LookupFile`, refused over 2 MiB or with a NUL byte in its first 8,000 bytes, its lines counted to check and cut the span, then `Blame`, each distinct commit of the ranges read once and built by `commitObjects` without `stats`; `search.go` serves the search route: `q` checked (1 to 500 characters, no NUL) with the strict booleans `regex` and `case_sensitive`, then `Grep` from the resolved commit, the `path` globs applied by `glob.go`, each line cut to its first 500 characters (runes, not bytes), git stopped once one match past the page is read, and `gitrepo.ErrInvalidPattern` answered `400 invalid_request` |
| `internal/scrub` | the one scrubbing function for text that leaves the hub: `Scrubber.Scrub` replaces registered secret values and the hub's own credential formats by `[redacted]`, removes user information from URLs and cuts the text to 1024 bytes; a leaf package that imports nothing of the project |
| `internal/secret` | the hub's secrets, whose values leave the hub through no response, log line or error: `LoadKeys` reads the operator's current key (`PARLEY_SECRETS_KEY`, or the file `[secrets].key_file` names) and previous key (`PARLEY_SECRETS_KEY_PREVIOUS`), each standard padded base64 of 32 bytes, into `Keys`, whose keys print, log and encode as JSON as `[key]` (an absent key encodes as `null`); `Seal` and `Open` seal a value with AES-256-GCM under the current key, a fresh 12-byte nonce and a binding of its owner as additional data, and open it with the current or the previous key by the `KeyID` (the first 8 bytes of the key's SHA-256 in hex) it carries; `SealValue`, `OpenValue` and `NeedsReseal` do the same for values that are not named secrets, with the key id in front of the sealed bytes; `Component` (the hub uses its zero value, bound once at start by `Bind(Deps)` to the store, the keys, the clock, the scrubber and the event log; `New(Deps)` builds a bound one; table `secrets`, created by `SchemaStep`, unique on scope, scope id and the key with A to Z lowered) is the one place secrets are read, stored and removed, in the scopes `ScopeUser`, `ScopeOrg` and `ScopeWorkspace`: `Get` reads one secret of exactly one scope on every call, matching the key ignoring ASCII case, registers the value with the scrubber and fails a value no key opens with `ErrUnreadable`, never as absent; `Put`, `Delete`, `RemoveScope` and the package function `RemoveWorkspace` change secrets inside the caller's transaction and write no event; `CreateEntries` (all or nothing, the limit of 100 checked before a conflict), `ListPage`, `UpdateEntry`, `DeleteEntry` and `Count` serve the secret routes; `Routes` are the secret routes and `ScopeCheck` the access check that resolves a route's scope from its `MarkScope` mark; `Available` reports whether a current key is configured; `StartReport` counts the stored values whose key id is neither configured key's and `(StartReport).Log` writes the start lines; `Reencrypt` is the maintenance task that re-seals the values under the previous key with the current one and runs the `Converter`s other parts add with `RegisterConverter`; `ErrUnavailable` (no current key), `ErrUnreadable` (no configured key opens the value), `ErrInvalid`, `ErrLimitReached`, `ErrConflict` and `ErrNotFound` are the errors to match, and `UnavailableError` is the `503 secrets_unavailable` the REST API answers for `ErrUnavailable`; the same component keeps variables, readable values in the same three scopes (table `variables`, `VarSchemaStep`): `CreateVars`, `ListVars`, `UpdateVar`, `DeleteVar` and `VarRoutes` serve the variable routes, `RemoveVarScope` and `RemoveWorkspaceVars` remove a scope's variables inside the caller's transaction, `Chain` is a workspace's resolution chain (`Step`, `Origin`, `ErrWorkspaceNotFound`), `ResolveVars` the variables it runs with and `ResolveSecrets` the secrets of a list of names along it, `ResolveSecretsIn` the same with the chain's records read through the caller's transaction |
| `internal/session` | the agent sessions: the session record, its token usage and its links to forks, change versions, pushes, commits, claims, lands and sandboxes (`SchemaStep`), the routes `POST /sessions`, `GET /sessions`, `GET /sessions/:id`, `POST /sessions/:id/complete`, `POST` and `GET /sessions/:id/usage` and `GET /workspaces/:slug/cost`, the session-linking rule (`Check`, `ErrUnknownSession`), the recording operation (`Record`, `Link` and its constructors), the forced close (`CloseActive`), the usage observers and active-session counts for the metrics (`OnUsage`, `ActiveCounts`), and the run id of a session for the record of sandbox use (`RunOf`); see [Agent sessions](#agent-sessions) |
| `internal/telemetry` | agent telemetry: run events, session outcomes, tool calls, tool errors and trace records stored once per id, and one postmortem per workspace and run, in the analytical store (`SchemaSteps`); the eight ingestion routes under `/workspaces/:slug/runs/:run_id`, single and in batches; the key `[audit].write_timeout` (`ConfigKey`, `WriteTimeoutFrom`); the recorder of sandbox use (`RecordSandboxUse`, `SandboxExec`, `SandboxAttach`, `SandboxUpload`, `SandboxDownload`); and the run event observers (`OnRunEvent`); see [Agent telemetry](#agent-telemetry) |
| `internal/store` | the hub's two embedded SQLite stores (`modernc.org/sqlite`, no cgo): `Open` creates the data directory and the workspace root, opens the transactional store and the analytical store (`analytics.db` beside it) and applies the registered schema steps; `Tx` runs a transaction of the transactional store; `Snapshot` writes a consistent copy of one store file without changing it |
| `internal/server` | the hub's HTTP server: its built-in routes, the REST API's route registration and check pipeline, the error envelope, request ids, response headers and the access log, list paging, idempotency keys, and the readiness probes |
| `internal/server/timefmt` | the one timestamp format (`Format`, and the response member type `Time`) and the one duration grammar (`ParseDuration`); a leaf package that imports nothing of the project, so that the configuration loader can use it as well as the REST API |
| `internal/workspace` | the place for workspace records, their validation rules, the state guard, and the create and clone work; so far the whole workspace record with its store functions, the state guard `RequireReady`, the resolution of a workspace's origin credential from its secrets `ResolveOriginCredential`, the workspace service with `POST /api/v1/workspaces`, `GET /api/v1/workspaces`, `GET /api/v1/workspaces/:slug`, `PATCH /api/v1/workspaces/:slug`, `POST /api/v1/workspaces/:slug/sync`, `POST /api/v1/workspaces/:slug/branches/:name/push`, `POST /api/v1/workspaces/:slug/archive` (see Archive), `POST /api/v1/workspaces/:slug/reactivate` (see Reactivate) and `POST /api/v1/workspaces/:slug/reclone` (see Reclone) and `DELETE /api/v1/workspaces/:slug` (see Delete) and the clone work `Clone` with the copy's place `RepoPath` and the post-clone steps, the sync of a workspace with its origin `(*Service).Sync` (see Sync), the safety copy that offers every branch to origin before an archive or a reclone (see Safety copy), the maintenance marker an archive, a reclone and a delete hold with `ClearMarkers`, which releases every marker at start, and the teardown set `Teardown` with `RemoveOwned`, through which an archive and a delete reach the data other packages keep for a workspace (see Archive), the clone workers `Workers` and `(*Service).Enqueue`, which hands a workspace put back to `pending` to them (see Clone work), the one encoder of the workspace object `RenderWorkspace` and the settings registry with the settings table of the 21 setting keys and `RegisterSettingKeys` (see Workspace records), the one branch-glob matcher `MatchBranchGlob`, `MatchAnyBranchGlob` and `ValidateBranchGlob`, and the rules every workspace route judges its values by, each deciding on the text alone and returning an error that says which rule broke: `ValidateSlug` (3 to 64 characters of a to z, digits and hyphens, starting with a letter), `ValidateGitURL` (`https://<host>/<path>` without user information, or `git@<host>:<path>`, with no lookup or connection), `ValidateBranchName` (git's branch-name rule), `ValidateForkName` (1 to 32 characters of a to z, digits and hyphens) and `ParseDuration` (a whole number above zero followed by `m`, `h` or `d`, unlike `timefmt.ParseDuration`, which also takes seconds) |
| `test/e2e` | tests that build the binaries and run them |

## The HTTP server

`server.New(server.Options)` builds the server, and `(*Server).Handler()`
returns the `http.Handler` that serves its routes. `Options` holds the listen
address (`Addr`) or a listener already bound (`Listener`), the shutdown grace (`ShutdownGrace`), the readiness
probes (`ReadinessProbes`) and `OnListen`, a function `Serve` calls once with
the address it bound (with port `0` in `Addr`, the port the system chose),
the REST body limit (`MaxBodySize`), the rate limit (`RequestsPerMinute`,
`RateLimitDisabled`, and `Now`, the clock it and the idempotency records
read, which tests replace), the `Logger` (a `*slog.Logger`; nil means
`slog.Default()`), `LogHealthProbes` and the `IdempotencyStore` (nil means
an in-memory store).
`Serve` serves `Handler()`, so the running hub and in-process tests answer
the same way.

The built-in `GET /healthz`, `GET /readyz` and `GET /version`, and the
surfaces outside the REST API that later packages mount with
`(*Server).Handle(pattern, handler)`, are registered on one standard library
`http.ServeMux` with its method and path patterns; `Handle` bypasses the REST
API's checks. The REST API routes are held in the server's own route table
and matched by the dispatcher (below), because the mux refuses two patterns
of which neither is more specific, such as `/changes/by-commit/:sha` and
`/changes/:number/stack`. Success bodies are
written with `server.WriteJSON`, errors with `server.WriteError`, both with
`Content-Type: application/json; charset=utf-8`. The routes are described in
[api.md](api.md).

### Request ids, headers and the access log

`Handler()` returns the dispatcher below wrapped in one outermost middleware
(`internal/server/headers.go`), so it covers every surface: the built-in
routes, the REST API, unknown paths and surfaces mounted with `Handle`. Before
anything else runs it:

1. takes the request id from an incoming `X-Request-ID` that is a UUID in the
   36-character hyphenated hexadecimal form (any version, either case),
   unchanged, or else generates a random version 4 UUID from `crypto/rand`;
   puts it in the request's context, where `server.RequestID(ctx)` reads it;
2. sets `X-Request-ID`, `X-Content-Type-Options: nosniff`,
   `X-Frame-Options: DENY` and `Referrer-Policy: no-referrer` on the response;
3. sets `Cache-Control: no-store` when the path is `/api/v1` or under it. The
   pipeline replaces it with the matched route's `CacheControl` when the route
   states one; the `/healthz` and `/readyz` handlers set `no-cache` and the
   `/version` handler `public, max-age=300`.

It hands the request on with a `ResponseWriter` that records the status. The
wrapper keeps `Unwrap()`, so `http.ResponseController` (flush, deadlines)
still reaches the connection's writer. When the request is answered, it writes
the access-log line (`internal/server/accesslog.go`) through `Options.Logger`:
message `request` with `method`, `path` (`r.URL.Path`, never the query
string, so a credential sent as a query parameter never reaches the log),
`status`, `duration_ms` and `request_id`, at info level. Requests to
`/healthz` and `/readyz` get a line only when `Options.LogHealthProbes` is
set, and then at debug level.

### Dispatch

The dispatcher sits in front of the mux. The mux alone would
redirect a path that is not clean (`//x`, `/a/../x`) or that lacks the
trailing slash of a subtree pattern, and would answer an unknown path or a
wrong method in plain text. The dispatcher answers a path that is not clean
with the `404 not_found` envelope.

A path under `/api/v1/` is matched against the REST routes first, segment by
segment on the unescaped segments: a literal segment must be equal, a
parameter takes any segment that is not empty. Of the routes that match the
path and are registered for the method (or for `GET` when the method is
`HEAD`), the most specific serves it: reading left to right, the first
segment where one pattern has a literal and the other a parameter decides,
the literal winning, so `/changes/by-commit/stack` goes to
`/changes/by-commit/:sha` and `/changes/7/stack` to `/changes/:number/stack`.
Of a `GET` and a `HEAD` route with the same pattern, a `HEAD` request goes to
the `HEAD` route. When routes match the path but none is registered for the
method, the answer is `405 method_not_allowed` with their methods, sorted, in
`Allow`. When none matches, the path goes on to the mux.

For every other path the dispatcher asks the mux (`ServeMux.Handler`) which
pattern matches, and answers no match with the `404 not_found` envelope, or
with `405 method_not_allowed` and the mux's `Allow` list when the path is
registered under other methods. It removes the `HEAD` that the mux adds for
every `GET` pattern unless a `HEAD` pattern exists for the path. A redirect
the mux would send for a missing trailing slash is a `404` too. Everything
else goes to `ServeMux.ServeHTTP`.

### REST routes

A feature registers a REST route with `(*Server).Register(server.Route)`.
`Route.Method` is an upper-case method, `Route.Pattern` a path relative to
`/api/v1` with `:name` parameters (`/workspaces/:slug/tokens`) and
optionally a last `*name` parameter that takes one or more remaining
segments (`/workspaces/:slug/scm/refs/*name`), or, for a route that sets
`EmptyCatchAll`, also the empty remainder of a path that ends in a slash
where the parameter begins (the forget of remembered resolutions, whose
handler answers an empty pathspec `400`), and
`Route.Handler` a `server.HandlerFunc`,
`func(http.ResponseWriter, *http.Request) error`: it writes its own response
on success, and an error it returns is written as the envelope. The route's
declarations are `Public`, `Create` with its `ShowOnce` members, its own
`MaxBodySize`, `ContentType` and `CacheControl`, `StreamBody` (the handler reads the body itself), its `Feature` check, the one
`Scope` a personal access token needs (a name the server does not interpret)
and `Marks`, named values whose meaning the installed checks define.
`Register` adds the route to the server's route table and panics on a
pattern that does not start with a slash, ends in one, has an empty, `.` or
`..` segment, a brace, a `*name` parameter that is not the last segment, or
a parameter without a valid name or named twice, on
a method that is not upper-case letters, on a missing handler and on a route
already registered, which includes one that differs from a registered route
of the same method only in its parameter names.

Each check and the handler read the route their request matched with
`server.MatchedRoute(r)`: a `server.Matched` with the method and pattern as
registered, the `Route` and the path parameter values (`Params`);
`server.Param(r, name)` returns one value; a `*name` parameter's value is
the unescaped segments it took, joined by `/`. Of two routes that match a
path, the one whose first differing segment is a literal beats a
parameter, and a parameter beats a `*name` parameter; when one pattern is
the beginning of the other, the longer wins.

### The check pipeline

After the mux has matched a REST route, the pipeline runs the route's checks
in a fixed order and stops at the first error:

1. authentication, through the installed `server.Authenticator`,
   `func(*http.Request) (server.Caller, error)`; with none installed, a route
   that is not public is answered `401 unauthenticated`;
2. the rate-limit stage (`internal/server/ratelimit.go`): it counts the
   request against the caller's credential id, or against the connection's
   remote IP address on a public route, and answers `429 rate_limited` with
   `Retry-After` when the key is over the limit;
3. the body stage (`internal/server/body.go`): it reads the whole body, up to
   one byte past the limit that applies (the route's `MaxBodySize`, else
   `Options.MaxBodySize`, else 1048576 bytes), answers `413
   payload_too_large` when the declared length or the bytes read exceed it,
   then, for a non-empty `POST`, `PUT` or `PATCH` body, answers `415
   unsupported_media_type` unless the `Content-Type` media type is the
   route's `ContentType` or, when it states none, `application/json`. The
   buffered bytes replace the request's body and stay in its context, so a
   later stage can read them without consuming the body the handler reads.
   For a route that sets `StreamBody` the stage reads nothing and checks
   neither the size nor the content type: the handler streams the body and
   enforces its own limit and media types;
4. the route's `Feature` check, the binding guard (`SetBindingGuard`), the
   scope check (`SetScopeCheck`) and the access checks (`AddAccessCheck`), in
   that order; each is a `server.Check`, `func(*http.Request) error`, and an
   empty position passes;
5. on a route declared `Create`, the idempotency stage
   (`internal/server/idempotency.go`), described below;
6. the handler.

A public route skips authentication and step 4. The access position holds a
list: every check added with `AddAccessCheck` runs, in the order it was
added, and decides from the matched route whether it applies. The
authenticator reports a `server.Caller` (`CredentialID`, and `UserID` or
`Admin` for the admin token, and `Detail`, what the authenticator knows
beyond the ids, which the server does not read), which the pipeline puts in
the request's context; later checks and the handler read it with
`server.CallerOf(r.Context())`. A check returns only an error and cannot
hand on a request of its own, so the router's handler of each REST route
puts an empty holder in the request's context before the pipeline runs
(ADR 04): a check leaves a value for the later checks and the handler with
`server.Attach(r, key, value)`, and they read it with
`server.Attached(r, key)`, which reports `false` when nothing was attached
under `key`; outside a request to a REST route `Attach` does nothing and
`Attached` reports `false`. Each request has its own holder. An `*server.Error` (also when wrapped) is
written as its envelope; any other error as `500 internal` with the message
`internal error`, and its text is logged through `Options.Logger` as an
error line `internal error` with the `request_id` and the `error`. When the
handler had already started its answer, nothing is written after it: the
error is logged as `error after the answer began` with the `request_id` and
the `error`. The
pipeline also recovers a panic in a check or the handler: it logs an error
line `panic` with the `request_id`, the panic value as `error` and the
`stack`, and answers `500 internal` unless the handler had already started
its answer. A panic with `http.ErrAbortHandler` is passed on, so net/http
drops the connection as the handler asked. A panic on a surface outside the
REST API is not recovered: net/http drops that connection, and the access
log records the request with status `500`.

A request that authentication refuses, also for want of an authenticator,
is counted against its remote IP address and then answered with
authentication's error: authentication comes before the rate limit in the
order of checks, so the address's count decides the answer only on public
routes.

The rate limit is a `server.Limiter`: `NewLimiter(perMinute, now)` counts
requests per string key in fixed windows of 60 seconds, each starting at the
key's first counted request, and `(*Limiter).Allow(key)` reports whether a
request is admitted and, when not, the whole seconds to the window's end
(rounded up, at least 1). A refused request does not change the window, and
a limit of 0 admits everything. It is safe for concurrent use, keeps its
counts in memory only, and drops ended windows at most once a minute. The
server builds one from `RequestsPerMinute` (0 means 600) unless
`RateLimitDisabled` is set, and `(*Server).RateLimiter()` returns it so that a
surface mounted with `Handle`, such as the git server, counts against the same
keys. Its keys are `credential:<id>` (a caller with a credential id) and
`address:<ip>` (no caller, or a caller without a credential id);
`server.LimitKey(r, credentialID)` returns the key of a request, the address
key when `credentialID` is empty, and is what both the pipeline and a
`Handle` surface count with. Built-in routes and `Handle` surfaces do not
pass the pipeline, and `404`/`405` are answered by the dispatcher before it,
so none of them is counted by it; the git server counts its own requests
(see [Git server](#git-server)).

A handler decodes a JSON body with `server.DecodeJSON(r, &v)`, which
returns `400 invalid_request` for an empty body, invalid JSON, a value that
is not an object or data after the object, and ignores unknown members.

A list handler pages through `internal/server/list.go`:

1. `server.ReadPage(r, bounds...)` reads `limit` and `cursor` from the query
   and returns a `server.Page`, or a `400 invalid_request` error that the
   handler returns before it reads its collection. The limit defaults to 50
   within 1–200; a route with other bounds passes its own
   `server.PageBounds{Default, Min, Max}` as the argument (there is no `Route`
   field for it).
2. `(server.Page).After(&pos)` decodes the position the page continues after
   into a value of the handler's own type, the keyset of its total order, and
   reports false on the first page.
3. The handler fetches up to `page.Limit+1` items after that position and
   calls `server.WritePage(w, page, items, position)`, which writes the first
   `Limit` items as `items` (`[]` for none) and, when the extra item exists,
   `next_cursor` from `position` of the last item written, else `null`. A
   list that names more than its items calls
   `server.WritePageWith(w, page, items, position, members...)`, which writes
   each `server.Member{Name, Value}` after `next_cursor`, in order.

A cursor is the unpadded URL-safe base64 form of the JSON document
`{"p": <position>, "f": <fingerprint>}`. The fingerprint is a truncated
SHA-256 of the matched route's method and pattern as registered, its path
parameter values and every query parameter except `cursor` and `limit`, so a
cursor is refused on another query but accepted with another limit. Cursors
are not signed: a cursor grants nothing, since every page passes the full
order of checks again.

The idempotency stage runs on create routes only, after every check, so a
replay is authenticated and authorised like the request it repeats. A
request without an `Idempotency-Key` header, every request to a public
route, which has no caller to record for, and a request whose caller has no
admin flag, user id or credential id, goes straight to the handler. A key that is empty or longer
than 255 characters (counted in characters, not bytes) is `400
invalid_request`. Otherwise the record is identified by three strings: the
key, the caller (`admin-token` when `Caller.Admin`, else `user:<UserID>`,
else `credential:<CredentialID>`) and
the route (the method, one space and the concrete `r.URL.Path`, such as
`POST /api/v1/workspaces/w1/tokens`). The stage then:

1. takes the per-record gate for that identity (below);
2. finds the record; one whose `WrittenAt` is 24 hours or more before
   `Options.Now()` is treated as absent. A valid record with the SHA-256 of
   the buffered request body is replayed as `200` with its `Header` and its
   body; one with another hash is `409 conflict`, and the record is left as
   it is;
3. otherwise runs the handler through a writer that passes the response
   through unchanged and keeps a copy of its status, its body and the
   headers the handler set before it answered: those it added or changed,
   without `Content-Length`, since the recorded body can be shorter (it has
   `Unwrap()`, so the status-capturing writer of the headers middleware below
   it and `http.ResponseController` keep working);
4. when the handler returned no error and answered `2xx`, removes the route's
   `ShowOnce` members from a copy of the body (a member name, or a dotted path
   into nested objects such as `token.token`; a path the body lacks is
   skipped; an object it changes is re-encoded with its members in sorted
   order and without white space between them, and every value kept is
   unchanged), calls
   `DeleteExpired` with the cutoff `now - 24h` when no sweep has started in
   the last minute (a sweep of the in-memory store visits every record, so
   it does not run on every write), and then `Write`s the record.
   The sweep and the write run on the request's context without its
   cancellation and with a 10 s deadline of their own: a handler may carry
   its create out after the client left (a forge write does), and a retry
   must still find the record.
   A body that is not a JSON object while `ShowOnce` names members, or a store
   error, is logged as `idempotency record not written` with the
   `request_id` and the `error`, and nothing is recorded: the caller already
   has the handler's answer;
5. records nothing for any other status or for an error.

The records live behind `server.IdempotencyStore`, which `internal/hub`
implements over the transactional store and installs as
`Options.IdempotencyStore`:

```go
type IdempotencyStore interface {
	Find(ctx context.Context, key, caller, route string) (IdempotencyRecord, bool, error)
	Write(ctx context.Context, rec IdempotencyRecord) error
	DeleteExpired(ctx context.Context, cutoff time.Time) error
}

type IdempotencyRecord struct {
	Key         string            // the Idempotency-Key, 1–255 characters
	Caller      string            // "admin-token", "user:<id>" or "credential:<id>"
	Route       string            // "POST /api/v1/..." with the concrete path
	RequestHash [sha256.Size]byte // SHA-256 of the request body's bytes
	Body        []byte            // the response body without its show-once members
	Header      http.Header       // the headers the handler set, without Content-Length
	WrittenAt   time.Time         // the server's clock when written
}
```

- `Find` returns the record whose `Key`, `Caller` and `Route` are all equal
  to its arguments, and `false` when there is none. It may return an
  expired record that is not deleted yet; the server decides from
  `WrittenAt` and never replays it.
- `Write` stores the record, replacing any record with the same `Key`,
  `Caller` and `Route` (a key whose record expired is recorded anew). A
  store keeps every field, `Header` too: a replay sends only the headers
  the record holds.
- `DeleteExpired` removes every record whose `WrittenAt` is at or before
  `cutoff`, that is, every record 24 hours old or older.
- An error from `Find` ends the request as `500 internal`; an error from
  `DeleteExpired` or `Write` is logged, as above. Every method must be safe
  for concurrent use. The stage calls only these three methods and keeps no
  record of its own.

`server.NewMemoryIdempotencyStore()` is the in-memory implementation and the
server's default; its records are lost when the process ends. `parleyd` uses
the store-backed implementation of `internal/hub` instead (see
[The hub](#the-hub)).

The gate that orders requests with one identity is in memory in either case,
since the hub is one process: a map from (key, caller, route) to a slot with
a one-token channel and a count of the requests holding or awaiting it. A
request takes the token before it reads the store and gives it back after the
record is written or the request has failed, so a request waiting on the same
identity sees the record when the first one succeeded, and exactly one waiter
runs the handler when it failed. The map's mutex is held only to find or drop
a slot, never while a request waits or runs, so requests with different keys,
callers or paths never wait for each other; a slot is dropped when its count
reaches zero. A waiting request whose client goes away (its context ends)
stops waiting and ends unanswered: nothing is written, nothing is logged at
error level, and a debug line `request ended while waiting for idempotency key`
carries the `request_id` and the `error`. Its access-log line shows status
`200`, the status of a request that wrote nothing.

The installed checks are copied under a read lock at the start of each
request, and no lock is held while a check or a handler runs, so a request
blocked in its handler never holds up another one.

### Timestamps and durations

Timestamps and durations are written and read in `internal/server/timefmt`,
not in `internal/server` itself, because the package imports nothing of the
project: the configuration loader can parse its duration keys with it
without importing the HTTP server. `timefmt.Format(t)` writes an instant as
RFC 3339 in UTC with the fraction of a second truncated. A response struct
declares a timestamp member as `timefmt.Time` (a defined type over
`time.Time`, converted with `timefmt.Time(t)`), whose `MarshalJSON` writes
the same string, so no handler formats a timestamp by hand.
`timefmt.ParseDuration(s)` reads a whole number with one of the units `s`,
`m`, `h` or `d`; it checks the digits itself, because `strconv` would accept
a sign, and refuses a value whose product with the unit would overflow a
`time.Duration`. No route or configuration key uses the package yet.

### Serving and stopping

`(*Server).Serve(ctx)` serves on `Listener` when it is set, and otherwise
binds `Addr` with `net.Listen("tcp", …)` and returns the error
(`listen on <addr>: …`) when it cannot; it calls `OnListen` and serves until
`ctx` is cancelled. Then it stops in this order:

1. It closes the listener, so new connections are refused.
2. It waits up to 100 ms for connections that are accepted but whose request
   has not been read yet. `http.Server.Shutdown` would drop those, and a
   request the client had already sent would get no answer.
3. It calls `http.Server.Shutdown` with a deadline of `ShutdownGrace`, which
   waits for in-flight requests to finish and closes idle connections.
4. When the grace passes first, it calls `http.Server.Close`, which closes the
   remaining connections; their clients get no response. `Serve` does not
   wait for those handlers to return.

`Serve` returns `nil` after a stop, whether or not the grace passed.

## The stores

`store.Open(ctx, store.Options{DatabasePath, WorkspaceRoot}, steps)` returns a
`*store.Store`. It:

1. refuses a step list with a step without a name or a function, with an
   unknown target, or with a name given twice, before it creates anything;
2. creates the data directory (the directory of `DatabasePath`) and
   `WorkspaceRoot`, with missing parents, with mode 0700; a directory that
   exists keeps its mode;
3. refuses a store file that exists, is not empty and does not begin with
   SQLite's header, by reading it only, so the file keeps its bytes; both files
   are looked at before either is opened;
4. opens the transactional store at `DatabasePath` and the analytical store at
   `store.AnalyticsFile` (`analytics.db`) in the same directory, each with
   `busy_timeout` 10000 ms, `journal_mode` WAL, `foreign_keys` on and
   `_txlock=immediate` (every transaction takes the write lock when it
   begins);
5. applies, in registration order, every step its target store (`store.Main`
   or `store.Analytics`) has not recorded in its `schema_steps` table, each in
   one transaction together with its record. A step that returns an error or
   panics is rolled back, no later step runs, and `Open` fails with an error
   naming the step; the steps before it stay applied and are skipped on the
   next open.

Every failure returns no store and an error naming the path or the step.

A `store.Step` is `{Name, Target, Apply func(ctx, *sql.Tx) error}`. Its name
is what the store records, so a step is never renamed.

`(*Store).Tx(ctx, fn)` runs `fn` in a transaction of the transactional store:
it commits when `fn` returns nil, and rolls back when `fn` returns an error
(returned as it is) or panics (returned as an error carrying the panic value).
Transactions of `Tx` take one token in turn, so the process's writers wait for
each other in memory and never fail with "database is locked"; `fn` must not
call `Tx` again. `Main()` and `Analytics()` return the two `*sql.DB` handles:
the first for reading, the second for the packages that read and write the
analytical store directly. `Ping` returns nil when both stores answer
`SELECT 1`, and an error naming the store that did not. `Close` closes both;
a second `Close` returns nil, and `Tx` and `Ping` return `store.ErrClosed`
after it.

`store.Snapshot(ctx, src, dst)` writes a copy of the store file `src` to the
new file `dst` with `VACUUM INTO`, which reads the source in one read
transaction: the copy holds every transaction committed before it began,
each whole, while other connections, of this process or another, go on
committing, and it has no write-ahead log. The source is opened read-only
(`mode=ro`), so the engine never checkpoints its write-ahead log. A source
without a write-ahead log (a store no connection has open) is read with
`immutable=1` as well, because a read-only connection would leave an empty
`-wal` and a `-shm` file beside it; when a log appears or the file changes
during that copy, the copy is discarded and taken again read-only. `dst`
must not exist, and a failure removes what was written of it.

## The hub

`internal/hub` composes the `parleyd` process, so that `cmd/parleyd` stays a
thin `main` and tests start a whole hub in-process. `hub.Run(ctx,
hub.Options)` returns the exit status. `Options` holds the process's inputs,
`Args`, `Env` (a `config.Env`, the shape of `os.LookupEnv`), `WorkDir`,
`Stdout` and `Stderr`, and the fields tests set and `parleyd` leaves zero:
`Compose` (parts added after the hub's own), `Port` (replaces
`[server].port`), `Listener` (a bound listener served on in place of binding
`[server].bind` and the port, which the hub closes when it stops), `Now` (the hub's clock), `RequestGrace` (replaces
`hub.DefaultRequestGrace`, 15 seconds), `MaintenanceInterval` (replaces
`maintenance.Interval`, one hour, the time between two maintenance passes),
`StopLimit` (replaces `hub.DefaultStopLimit`, 30 seconds, the longest a stop
waits) and `LoopGrace` (replaces `hub.DefaultLoopGrace`, 15 seconds, how long
a stop waits for the maintenance loop). `cmd/parleyd`'s `run(ctx, args, env,
wd, stdout, stderr)` builds the options and calls `Run`; `main` passes
`os.Args[1:]`, `os.LookupEnv` and the working directory.

### The composition

A part is a `func(*hub.Composition)`. The hub's own parts are listed, in
order, in the `parts` variable of `internal/hub/hub.go`; a package that plugs
into the hub writes its part in a file of its own in `internal/hub` and
appends it there ([ADR 01](adr/01-compose-the-hub-in-one-package.md)).
`Options.Compose` runs after them. On the composition a part registers:

| Method | What |
|---|---|
| `RegisterKey(config.Key)`, `RegisterSection(name, validate)` | configuration keys and whole sections |
| `AddSchemaStep(store.Step)` | schema steps, applied in the order they were added |
| `AddStartHook(name, func(ctx, *hub.Runtime) error)` | start work, run in order, each to completion |
| `AddMaintenanceTask(name, func(ctx, *hub.Runtime) error)` | maintenance tasks (see [The maintenance loop](#the-maintenance-loop)) |
| `AddBackgroundWork(name, grace, func(ctx, *hub.Runtime))` | work that runs while the hub serves, until its context is cancelled at the stop (see [The stop](#the-stop)) |
| `AddRoute(server.Route)`, `Handle(pattern, handler)` | REST routes and surfaces outside the REST API |
| `SetAuthenticator`, `SetBindingGuard`, `SetScopeCheck`, `AddAccessCheck` | the checks of the pipeline's positions |
| `SetSandboxRuntime(sandbox.RuntimeFactory)`, `SetSandboxRecorder(sandbox.Recorder)` | the factory of the sandbox runtime client (default: the OpenShell implementation) and the recorder of the sandbox metrics (default: none) |
| `SetSandboxPolicyCollector(sandbox.PolicyCollectorOptions)` | the policy collector's interval, retry delay and clock (default: 5 s, 5 s, the hub's clock), or `Hold`, which keeps the hub's collector from running so that a test drives its own |
| `AdjustWorkspace(func(*workspace.Deps))` | a change to the workspace service's dependencies, applied after the start hook `workspaces` builds them and before it binds them, in the order added; tests replace a collaborator with it |
| `AdjustGitServer(func(*gitserver.Deps))` | a change to the git server's dependencies, applied after the start hook `gitserver` builds them and before it binds them, in the order added; tests replace a collaborator with it, such as the git runner, the timeout or a service in place of the exchange |
| `AdjustSCM(func(*scm.Deps))` | a change to the source control reads' dependencies, applied after the start hook `scm` builds them and before it binds them, in the order added; tests replace a collaborator with it, such as the repository opener or the request recorder |
| `AdjustChange(func(*change.Deps))` | a change to the change component's dependencies, applied after the start hook `changes` builds them and before it binds them, in the order added |
| `AdjustLand(func(*land.Deps))` | a change to the land component's dependencies, applied after the start hook `land` builds them and before it binds them, in the order added |
| `AdjustAutoLand(func(*land.AutoLandDeps))` | a change to the automatic lander's dependencies, applied after the start hook `auto-land` builds them and before it binds them, in the order added; tests set its clock and interval with it |
| `AdjustPolicy(func(*policy.Deps))` | a change to the policy component's dependencies, applied after the start hook `policy` builds them and before it binds them, in the order added |
| `AdjustSandbox(func(*sandbox.Deps))` | a change to the stored sandboxes' dependencies, applied after the start hook `sandbox_records` builds them and before it binds them, in the order added; tests replace the minting component, the secret resolution or the readable set, set the clock's timers, change the sandbox limits while the hub serves (`Limits`), or hold the provisioner (`HoldProvisioning`) with it |
| `AdjustForks(func(*fork.Deps))` | a change to the fork component's dependencies, applied after the start hook `forks` builds them and before it binds them, in the order added; tests replace a collaborator with it, such as the name generator, the repository creator, the transaction or the event writer |
| `AdjustAwareness(func(*awareness.Deps))` | a change to the awareness component's dependencies, applied after the start hook `awareness` builds them and before it binds them, in the order added; tests set the index and expiry intervals or replace the event source, the change reader or the repositories with it |
| `AdjustCarryPatch(func(*carrypatch.Deps))` | a change to the carry-patch component's dependencies, applied after the start hook `carry_patch` builds them and before it binds them, in the order added; tests replace the secret reader or the git, or set the fetch deadlines, the branch-create hook and the reset's swap hook (`BeforeRefSwap`) with it |
| `Jobs()`, `AdjustJobs(func(*jobs.Deps))` | the hub's job queue, on which a part or `Options.Compose` registers the handler of a job type; and a change to its dependencies, applied after the start hook `jobs` builds them and before it binds them and recovers, in the order added; tests set its clock, its interval and `NoWake` with it |

`(*Composition).Sandbox()` returns the `*sandbox.Service` that
`composeSandbox` (`internal/hub/sandbox.go`) creates: a part listed after it,
or `Options.Compose`, keeps it and reads its settings and runtime client
once the hub serves. Unexported registrations serve the parts of the hub
itself: `addServerHook(func(*server.Server))` adds a function that receives
the HTTP server once `install` has registered the routes, mounts and checks
on it, for what only the built server has, such as its rate limiter
(`install` runs on a throwaway server while the composition is checked and
on the serving server after the start hooks, so what a hook keeps from the
second call is what requests see), `addConfigCheck(func(*config.Config) error)` adds a check run on the
loaded configuration before any store is opened, for rules that span more
than one key or section, `addVersionMember(func() map[string]any)` adds
members to the document of `GET /version` (the server's
`Options.VersionMembers`), after the build's, and
`addStartReport(func(*slog.Logger))` adds lines a part writes about its
start, which the hub writes once the listener is bound, right before the
`listening` line, so that a failed start still writes only its one error
line. The composition keeps the hub's environment (`Options.Env`, an empty
one when nil) in its unexported field `env`, which a configuration check
reads ([ADR 06](adr/06-load-the-secret-keys-before-the-stores-open.md)).

`(*Composition).Secrets()` returns the `*secret.Component` that
`composeSecrets` (`internal/hub/secrets.go`) creates: a part keeps it when
it is composed and reads, stores and removes secrets through it from its
start hook on, if it is listed after `composeSecrets`, or from
`Options.Compose` (see [Secrets](#secrets)).

`(*Composition).Events()` returns the `*event.Component` that `composeEvents`
(`internal/hub/events.go`) creates: a part keeps it when it is composed and
emits through it from its start hook on, if it is listed after
`composeEvents`, or from `Options.Compose` (see [Hub events](#hub-events)).

`(*Composition).Workspaces()` returns the `*workspace.Service` that
`composeWorkspaces` (`internal/hub/workspace.go`) creates, with the keys of
the settings table registered on `Workspaces().Settings()` by
`workspace.RegisterSettingKeys` (a failure panics the composition, as an
invalid route does): a part keeps it when it is composed (see
[Workspace records](#workspace-records)). A part that keeps data for a
workspace, or acts when one is archived or deleted, registers its
collaborator on `Workspaces().Teardown()`
([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)).

`(*Composition).Forks()` returns the `*fork.Component` that `composeForks`
(`internal/hub/fork.go`) creates: a part keeps it when it is composed and
asks it for a fork's refs, head and branch writes from its start hook on, if
it is listed after `composeForks`, or from `Options.Compose` (see
[Task forks](#task-forks)). The session part registers its session-linking
rule there with `RegisterSessionLinker` and the sandbox part its sandbox
component with `RegisterSandbox`, each while it is composed, from its own
file of `internal/hub`, as the teardown collaborators are registered on the
workspace service
([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)).
With sandboxes on, the sandbox part's start hook `sandbox_records` registers
the sandbox service as the sandbox component, after it binds it; the
sessions part registers the session-linking rule on it (see
[Agent sessions](#agent-sessions)).

The composition also keeps the identity service in its unexported field
`identity`, which `composeIdentity` sets. A part listed after it that keeps
data for a user or an org registers the remover of that data there, with
`c.identity.RegisterUserRemover` or `c.identity.RegisterOrgRemover`
([ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)).

`hub.Runtime` holds the loaded `*config.Config`, the open `*store.Store`, the
`*slog.Logger`, the `*scrub.Scrubber` and the clock `Now`. Start hooks,
maintenance tasks and background work receive it; a route handler or a check keeps
`(*Composition).Runtime()`, which the hub fills once the stores are open and
before the first hook runs or the first request is served. A start hook or a
maintenance task without a name or a function, or with a name already
registered, a configuration key or section the registry refuses, a
route the server refuses, and a route that breaks a route rule a part
added with the unexported `addRouteRule`, stop the start before anything is
opened; so does background work without a name or a function, or with a
name already registered. A route rule applies to every route of the
composition, including those `Options.Compose` adds after the hub's own
parts. `(*Composition).ShuttingDown()` returns the shutdown
notification, a channel the hub closes once when the stop begins.

### The start

`Run` parses the arguments with the standard `flag` package first: `--help`
prints the usage on stdout and returns `0`; an unknown flag,
`--admin-email` without a value, a positional argument other than `backup`,
`backup` without exactly one directory, and `backup` with `--admin-email` or
`--reset-admin-token` print the usage on stderr and return `2`. None of these
loads, opens or creates anything. `backup <directory>` runs
[the backup](#the-backup) instead of the start.

Then it builds the logger and starts in this order, stopping at the first
failure:

1. compose: the hub's parts, then `Options.Compose`;
2. load the configuration with the composition's registry (`config.toml`,
   see [configuration.md](configuration.md)), set the logger's level from
   `[logging].level`, run the configuration checks of `addConfigCheck`
   (the `[sandbox]` section's, see [The start-up check](#the-start-up-check),
   and the loading of the secret keys, see [Secrets](#secrets)),
   and register every OAuth provider's `client_secret` with the scrubber;
3. open the stores with the composition's schema steps (`store.Open`);
4. the bootstrap (`internal/hub/bootstrap.go`), which may end the run with
   `0` after writing the admin token file, or refuse the start (see
   [Bootstrap](#bootstrap));
5. the start hooks, in the order they were added, each to completion, with
   `Run`'s context;
6. catch `SIGTERM` and `SIGINT` with `signal.Notify` on a channel kept
   registered until `Run` returns, so that a second signal during the stop
   does not end the process;
7. bind `[server].bind`:`[server].port` with `net.Listen`;
8. build the server from `hub.ServerOptions(cfg, st)`, with the bound
   listener, the request grace, the clock and the logger, and install the
   composition's routes and checks;
9. start the maintenance loop in the background, without waiting for its
   first pass (see [The maintenance loop](#the-maintenance-loop)), and each
   piece of background work in a goroutine of its own, with one context
   shared with the server and the loop;
10. `Serve`, whose `OnListen` writes the parts' start reports
    (`addStartReport`), in the order they were added, and then the
    `listening` line with `addr`, `config` (`cfg.Path`) and `data_dir`
    (`cfg.DataDir()`).

The listener is bound before the maintenance loop starts, so that a bind
failure is the only line of a failed start. A failure before listening is
one line at error level, `cannot start` with the cause in `error`, or
`cannot listen` with `addr` and `error`; the stores are closed when they were
open, and `Run` returns `1`. The first signal, or the cancellation of `ctx`,
begins [the stop](#the-stop).

### The backup

`parleyd backup <directory>` (`internal/hub/backup.go`) composes the hub and
loads the configuration as the start does (steps 1 and 2), so the keys and
sections the parts register apply, and then only reads the data: it opens no
store for writing, applies no schema step, runs no bootstrap, start hook or
maintenance task, catches no signal and binds nothing. In order, stopping at
the first failure:

1. check that the database file, `analytics.db` and the workspace root
   exist, so that a backup run where no hub ever started creates no database;
2. check the target, a relative one taken relative to `WorkDir`, which
   `Run` makes absolute first: a directory inside the workspace root (which
   it would copy into itself) or inside the data directory (which a backup
   leaves untouched), a path that is not a directory and a non-empty
   directory are refused; a missing one is created with `os.Mkdir` and mode
   0700, its parent not;
3. copy the transactional store under the database file's own name and the
   analytical store as `analytics.db`, each with `store.Snapshot`;
4. copy the workspace root under its own name, keeping relative paths, file
   permission bits and symbolic links; the `objects` directory of each git
   directory (one that holds a `HEAD` file) is copied after the rest of the
   tree, so its refs, `packed-refs` and `HEAD` are read before its objects
   and every copied ref points at a copied object. A file, symbolic link or
   directory removed after its directory was listed and before it is copied
   (git creates and removes lock and temporary files while a hub runs) is
   left out, as if the snapshot were taken after its removal; any other
   error of the copy fails the backup.

A failure after the target was created or written to removes what the
backup wrote (the target itself when it created it, else its entries) and
logs one `backup failed` line naming the cause; `Run` returns `1`. Success
logs `backup written` with `dir` and returns `0`, with nothing on stdout.

### The stop

`internal/hub/shutdown.go` stops the hub on the first `SIGTERM` or `SIGINT`,
or when `Run`'s context is cancelled:

1. it cancels the context the server, the maintenance loop and the
   background work share, so the server closes its listener and stops
   accepting connections (see [Serving and stopping](#serving-and-stopping)),
   the loop starts no further task and cancels the running one, and every
   piece of background work sees its context done;
2. it closes the shutdown notification, before it waits for anything, so a
   handler of a long-lived response that watches `ShuttingDown()` ends it
   and the server need not wait out the request grace for it;
3. it waits for everything at the same time: the server, which gives the
   requests in flight the request grace and then closes their connections;
   the maintenance loop, up to `LoopGrace`; each piece of background work,
   up to its own grace, which `AddBackgroundWork` cuts to 30 seconds;
4. it stops waiting for anything once `StopLimit` has passed since the
   signal, and writes one `abandoned at the stop` warning, with `work`, for
   each thing it stopped waiting for; abandoned work keeps running and finds
   the stores closed;
5. it closes the stores, last, and `Run` returns `0`, whether or not a grace
   passed (`1` only when the server had failed while serving).

The signal channel stays registered until `Run` returns and nothing reads it
after the first signal, so a further signal is dropped: no grace starts
again and the process does not end early.

### The maintenance loop

`internal/maintenance` holds a `Registry` of tasks, each a name and a
`maintenance.Task` (`func(ctx) error`); `Register` refuses an empty name, a
nil function and a name registered before, with an error that names it, and
keeps the first task. The composition's `AddMaintenanceTask` registers on the
hub's registry, so a refused task is a start error like any other refused
registration.

`maintenance.New(reg, logger)` builds a `Loop` over the registered tasks.
`(*Loop).Run(ctx, ticks)` runs one pass at once and one at each tick it
receives; the hub passes the channel of a `time.Ticker` of
`maintenance.Interval`, one hour, which no configuration key changes
(`Options.MaintenanceInterval` replaces it in tests). A pass
(`(*Loop).Pass`) runs the tasks one after the other: those registered under
`retention`, `secret_reencryption`, `claim_cleanup`,
`forge_delivery_pruning`, `token_binding_sweep`, `patch_purge`,
`fork_expiry` and `sandbox_record_cleanup` in that order, a name nobody
registered skipped, then every task registered under another name in the
order it was registered. Passes never overlap: a tick that arrives while a
pass runs is received and dropped, and the next pass starts at the next
tick. A task that returns an error or panics is logged at error level as
`maintenance task failed` with `task` and `error` (its text passed through
the scrubber), and the pass goes on with the next task. When `ctx` is
cancelled the running task's context is done, no further task or pass
starts, and `Run` returns once the running task has returned; a task that
ends with an error after the cancellation is not logged as failed.

`hub.ServerOptions(cfg, st)` returns the options that come from the
configuration and the stores: `MaxBodySize` from `[server].max_body_size`,
`RequestsPerMinute` from `[limits].requests_per_minute` with
`RateLimitDisabled` set when it is `0` (the server reads a
`RequestsPerMinute` of 0 as 600), `LogHealthProbes` from
`[logging].log_health_probes`, one readiness probe, `st.Ping`, which fails
while either store does not answer a query, and the store-backed
`IdempotencyStore`.

### Bootstrap

`internal/hub/bootstrap.go` adds the part `composeBootstrap`, after
`composeIdempotency` in `parts`, which adds the schema step
`identity.AdminSettingsStep`: a table `admin_settings` with one row (`id` 1,
`email`, `token_hash`) that exists once the hub is bootstrapped.
`identity.AdminSettings(ctx, st)` returns the row as an `identity.Admin`, the
zero value when none is stored. `identity.GenerateAdminToken` returns
`parley_admin_` and 32 bytes of `crypto/rand` as 64 lowercase hex
characters; `identity.HashAdminToken` is the lowercase hex SHA-256 of the
whole token; `identity.VerifyAdminToken(ctx, st, token)` reports whether
the token's hash is the stored one, compared with
`crypto/subtle.ConstantTimeCompare`, and false when none is stored.
`identity.IssueAdminToken(ctx, st, email, deliver)` generates a token,
upserts its hash (the e-mail only when the row is new) and calls `deliver`
with the token inside the same `Tx`, so an error from `deliver` rolls the
hash back.

The bootstrap stage reads the settings and, by the table in
[cli.md](cli.md#the-admin-token), issues a token whose `deliver` writes it
to `<config dir>/admin_token` (a 0600 temporary file in the config dir,
synced and renamed over the path; a file already there is first given a
second name by a hard link, put back when the transaction then fails to
commit and removed once it has committed), refuses with an error naming the file
while a bootstrapped hub's file exists, or goes on to the start hooks. After
writing the file it logs the path at warn level and `Run` returns `0`
without running a start hook or binding the listener.

### Identity

`internal/hub/identity.go` adds the part `composeIdentity`, after
`composeBootstrap` in `parts`. It adds the schema steps
`identity.SchemaStep` (`identity_records`) and, after it,
`identity.PersonalOrgStep` (`identity_personal_orgs`), which adds the flag
`personal` to `orgs` and, in a store that has records already, marks the org
each user owns under their username, installs the `Authenticate`
method of an `identity.Service` as the pipeline's authenticator, installs
`identity.ScopeCheck` with `SetScopeCheck`, adds the service's
`AdminCheck` method with `AddAccessCheck`, registers every route of `(*identity.Service).Routes()` with `AddRoute`, and adds the
start hook `identity`, which binds the service with
`(*identity.Service).Bind(identity.Deps{Store, Now, Providers,
ExternalURL, Client})` from the `Runtime` before the hub serves its first
request. `Deps.Client` is the `identity.ProviderClient` a login reads the
user's identity through; the hub binds
`identity.NewOAuthClient(rt.Config.OAuth.Providers, http.DefaultClient,
identity.DefaultProviderTimeout)`, and a service without one answers every
login `502 upstream_error`.

The service's routes are, so far, the public `GET /auth/providers` (see
[api.md](api.md#get-apiv1authproviders)), the public `POST
/auth/callback` (see [api.md](api.md#post-apiv1authcallback)), which is not
a create route, the admin route `POST /users` (see
[api.md](api.md#post-apiv1users)), a create route, the admin routes `GET /users`, `GET /users/:id`
and `PATCH /users/:id` (see [api.md](api.md#get-apiv1users)), the admin
routes `POST /users/:id/promote`, `/demote`, `/block` and `/unblock` (see
[api.md](api.md#role-and-status-of-a-user)), the admin route
`DELETE /users/:id` (see [api.md](api.md#delete-apiv1usersid)), the admin
routes `GET /users/:id/keys`, `GET /users/:id/tokens`,
`DELETE /users/:id/keys/:key_id` and `DELETE /users/:id/tokens/:token_id`
(see [api.md](api.md#keys-and-tokens-of-a-user)), and the
caller's own profile, `GET /user`, `PATCH /user` and `GET /user/orgs` (see
[api.md](api.md#get-apiv1user)), which need a user, the admin routes
`POST /orgs`, a create route, `GET /orgs`, `PATCH /orgs/:id`,
`PUT /orgs/:id/members/:user_id`, `DELETE /orgs/:id/members/:user_id`,
`POST /orgs/:id/block`, `POST /orgs/:id/unblock` and `DELETE /orgs/:id`,
and `GET /orgs/:id` and `GET /orgs/:id/members`, which an admin or a member
reads (see [api.md](api.md#post-apiv1orgs)).
`(*identity.Service).Routes()` in `internal/identity/routes.go` returns
them all: it appends the route list of each handlers file, such as
`userRoutes()` in `internal/identity/handlers_users.go` and
`ownUserRoutes()` in `internal/identity/handlers_user.go` and `orgRoutes()` in
`internal/identity/handlers_orgs.go`, so the hub and the
tests register the same routes. The providers in
`identity.Deps.Providers` come with their endpoints resolved:
`internal/config` fills an endpoint the file does not give with the
provider's public one, through `config.Provider.WithPublicEndpoints`, and
holds the only table of those endpoints. `internal/identity/provider.go`
holds what the configuration does not: the providers the hub can log in
with, the scopes it asks each for and Google's extra `response_type=code`.

`internal/identity/oauth.go` reads a user's identity from a provider, with
the standard library only. `identity.NewOAuthClient(providers, hc,
timeout)` returns an `*identity.OAuthClient`, the `identity.ProviderClient`
whose `Identity(ctx, provider, code, redirectURI)` returns an
`identity.Identity` (`Provider`, `ProviderID`, `Username`, `FullName`,
`Email`). It exchanges the code with one form-encoded `POST` to the token
URL (`client_id`, `client_secret`, `code`, `redirect_uri`,
`grant_type=authorization_code`, `Accept: application/json`), then reads
the user URL with `Authorization: Bearer <access token>`. For GitHub the id
is the decimal `id`, the username `login` and the full name `name`; when
the profile's `email` is null or empty it reads the user URL with `/emails`
appended and takes the entry whose `primary` and `verified` are both true.
For Google the id is `sub`, `name` is both the username and the full name,
and `email` counts only when `email_verified` is true. The username is
trimmed, and an empty one becomes `<provider>-<provider id>`; the full name
is the provider's as given.
Every error is `identity.ErrRejected` (the token endpoint answered a JSON
object with an `error` member and a status below 500) or
`identity.ErrUpstream` (everything else: no connection, a timeout, a status
of 500 or above, a body that is not the expected JSON, no access token, a
user or e-mail list answer other than `200`, no user id, no verified
address), and its text is fixed words only, never the code, the client
secret, the access token or the provider's body. Each call to the provider
is bounded by the timeout (0 is `identity.DefaultProviderTimeout`, 15
seconds); when it passes, the request's context ends and the transport
closes the connection. `hc` nil is `http.DefaultClient`.

The schema step creates, in the transactional store:

| Table | Columns |
|---|---|
| `users` | `id` (primary key), `username` (unique), `email` (unique), `full_name`, `role` (`user` or `admin`), `status` (`active` or `blocked`), `provider` and `provider_id` (unique as a pair), `created_at`, `updated_at` |
| `orgs` | `id` (primary key), `name` (unique), `slug` (unique), `url`, `owner_id` (set to NULL when that user is deleted), `status`, `created_at`, `updated_at` |
| `memberships` | `org_id` and `user_id` (primary key as a pair; deleted with the org or the user), `created_at` |
| `api_keys` | `key_id` (primary key), `user_id` (deleted with the user), `secret_hash`, `expires_days`, `expires_at`, `revoked_at`, `created_at` |
| `pats` | `token_id` (primary key), `user_id` (deleted with the user), `secret_hash`, `name`, `permissions` (a JSON array), `paths` (a JSON array, NULL without path limits), `workspace` (NULL when not bound), `expires_at`, `revoked_at`, `created_at` |

Timestamps are nanoseconds since the Unix epoch, UTC; `expires_at` and
`revoked_at` are NULL when unset. `secret_hash` is the lowercase hex SHA-256
of the secret part of the credential; the secret is never stored.

`identity.IssueAPIKey(ctx, tx, userID, expiresDays, now)` and
`identity.StorePAT(ctx, tx, identity.PATSpec{UserID, Name, Permissions,
Paths, Workspace, ExpiresAt, CreatedAt})` generate a credential from
`crypto/rand` (an API key `parley_<8>_<32>` over `[0-9A-Za-z]`, a PAT
`parley_pat_<8>_<32>` over `[a-z0-9]`, each character drawn uniformly),
draw the id again while it is taken, store the record in the transaction
they are given and return the credential, which is shown once, with its
record (`identity.APIKey`, `identity.PAT`). An API key expires `expiresDays`
× 24 hours after `now`, never with 0. `StorePAT` validates nothing beyond
the record's shape. `identity.UserByID`, `identity.APIKeyByID` and
`identity.PATByID` read a record through an `identity.Querier` (the store's
`*sql.DB` or a `*sql.Tx`) and return `identity.ErrNotFound` when there is
none. `identity.User` marshals to the REST API's User object.

`(*identity.Service).Authenticate` reads the `Authorization` header (see
[api.md](api.md#authentication)), and `(*identity.Service).VerifyCredential(ctx,
credential)` checks the credential, the same check for a credential read
elsewhere: it recognises the format, verifies an admin token with
`identity.VerifyAdminToken`, or looks the key or PAT up by its id and
compares the SHA-256 of the presented secret with the stored hash with
`crypto/subtle.ConstantTimeCompare`, against a fixed dummy hash when the id
is unknown, so that an unknown id and a wrong secret take the same path.
Only after the secret matched does it look at the credential's state, in
this order: `revoked_at` set is `401 credential revoked`, the clock of
`identity.Deps.Now` at or after `expires_at` is `401 credential expired`,
and a user whose status is `blocked` is `403 user is blocked`. It reads the
record and the user from the store on every call and caches nothing.
It returns an `identity.Principal` (`Type` `admin`, `api_key` or `pat`,
`CredentialID`, the `User`, and a PAT's `Permissions`, `Paths` and
`Workspace`) or the `*server.Error` that answers the request. The
authenticator reports a `server.Caller` whose `CredentialID` is `admin`, the
`key_id` or the `token_id`, whose `UserID` is the user's id, whose `Admin`
marks the admin token, and whose `Detail` holds the `Principal`; checks and
handlers read it with `identity.PrincipalOf(r.Context())`. A route declares
that it needs a user with the mark `identity.MarkNeedsUser` in its
`server.Route.Marks`; the authenticator reads it right after accepting the
credential and answers the admin token `403 forbidden` there.

The login, in `internal/identity/login.go`, decodes the callback's body
and checks it before the provider is called: `provider`, `code` and
`redirect_uri` are non-empty strings, `provider` is one of
`Deps.Providers`, `expires` is absent, `null` or one of 0, 30, 60 and 90,
and `redirect_uri` passes `allowedRedirect` in
`internal/identity/redirect.go` (http on `localhost` or `127.0.0.1`, or the
scheme, host and port of `Deps.ExternalURL`; never with user information);
each violation is `400 invalid_request`. It then reads the identity through
`Deps.Client`, answering `identity.ErrRejected` `401 unauthenticated` and
`identity.ErrUpstream` `502 upstream_error`, each with the error's fixed
text, and signs the user in, in one
`(*store.Store).Tx`: it looks the user up with `identity.UserByProvider`,
and at a first login picks the username (the provider's, else the first
free of `<name>-2` to `<name>-10`) and the role (`admin` when the e-mail is
the `admin_email` of `admin_settings`, read in the same transaction,
ignoring letter case, and no user has the role `admin`), then calls
`identity.CreateUserAndPersonalOrg`. A user it finds is refreshed instead:
a blocked user is answered `server.Forbidden("user is blocked")`, an e-mail
another user has is an `ErrConflict`, a username another user has keeps
the old one, and the new e-mail, username and `updated_at` are written;
role, full name, creation time and the personal org are left alone. Then,
for a new and an existing user alike, it sets `revoked_at` on every API key
of the user with `revoked_at` NULL and `expires_at` NULL or after the
login time, and last it issues the API key with `identity.IssueAPIKey`,
`expires` days (90 when absent) from the clock of `Deps.Now`. Any error
rolls the whole transaction back.

`identity.CreateUserAndPersonalOrg(ctx, tx, identity.NewUser{Username,
Email, FullName, Provider, ProviderID, CreatedAt}, role)` is the one way a
user is created: it writes the active user, the user's personal org (an
`identity.Org`, active, named after the username, owned by the user and
marked `personal`) and
the membership in the transaction it is given, and returns the user and
the org. The org's slug comes from the pure function `slug(username,
userID)` in `internal/identity/slug.go`, or the first of `<slug>-1` to
`<slug>-10` no org has. User and org ids are UUID version 4 strings from
the standard library's `uuid`. It checks before it writes, and returns an
error that is `identity.ErrConflict` (`errors.Is`), writing nothing, when the
username, the e-mail or the provider identity is another user's, an org has
the username as its name, or no slug is free; a unique-constraint violation
of SQLite on the insert is reported the same way. The callback answers that
error `409 conflict` with its text. Its caller chooses the username and the
role.

The admin routes carry the mark `identity.MarkAdmin` (`identity.admin`) in
their `server.Route.Marks` and declare no scope. `(*identity.Service).AdminCheck`,
in `internal/identity/admincheck.go`, is an access check: on a route without
the mark it lets the request through; on one with it, it passes the
`identity.Principal` whose `Type` is `admin`, or `api_key` with a `User`
whose `Role` is `admin`, and answers every other caller, PATs included,
`server.Forbidden("this route needs an admin")`. It reads the role of the
`Principal` the authenticator built for this request from the store, so a
change of role holds from the next request on. Because it runs in the access
position, after the body's size and media type and before the handler, a
refused request is not validated, looks nothing up and changes nothing.

`internal/identity/scope.go` holds the scope registry and the scope check.
`identity.RegisteredScopes()` returns a copy of the 32 registered scopes in
their documented order, and `identity.IsRegisteredScope(s)` tells a
registered name from any other string, exactly. `identity.Satisfies(held,
needed)` is the one implication rule: `needed` itself, `R:write` for an
`R:read` where both are registered, `keys:manage` for `keys:read`, and
`tokens:manage` for `tokens:write` and `tokens:read`; nothing else, so
`changes:approve` and `checks:write` are granted only by name.
`identity.ScopeCheck` is a package-level `server.Check`, since it needs no
service state, which the hub installs in the pipeline's scope position. It
reads the `Scope` of `server.MatchedRoute(r).Route`, the one declaration a
route of any part makes, and the `identity.Principal` of the request: a
route without a scope, the admin token and an API key pass, and a PAT whose
`Permissions` do not satisfy the scope is answered
`server.InsufficientScope("this route needs the scope <scope>")`. A route
that declares a scope and whose request carries no `identity.Principal` (an
authenticator other than the identity service's) fails closed with
`server.Internal("the scope check found no authenticated principal")`
rather than passing. It runs before the access checks and the handler, so a
refused token learns nothing about the resource it addressed.

`internal/identity/pathglob.go` is the one implementation of a token's path
limits, pure functions with no state. `identity.ValidatePaths(v)` checks a
`paths` value as `encoding/json` decoded it (or a `[]string`): `nil` is no
path limit and returns `nil`; otherwise an array of 1 to 50 strings, each 1
to 255 characters and not starting with `/`, returned in the order given
with duplicates kept, or an error whose text names the entry it refuses
(`paths[1] is not a string`). `identity.MatchPath(glob, path)` matches the
whole path, case-sensitively: `*` is any run without `/`, `**` any run
including `/` (both may be empty, and both are wildcards wherever they
stand), a run of three or more asterisks reads as `**`, and every other
character, `?`, `[` and `\` included, is itself. `identity.MatchAny(globs,
path)` is the check of a token's list: a `nil` list allows every path,
otherwise one glob must match. The parts that refuse a write outside a
token's paths (git push, the SCM write API, opening changes and auto-land)
call `MatchAny` with the `Paths` of the request's `identity.Principal` and
match no glob of their own. `identity.Covers(c, g)` is the cover test that
minting holds a path-limited token to: `c` equals `g`, or `c` read as a
pattern matches the text of `g`, where a `*` of `c` takes no `/` and no
asterisk of a `**` in `g`, and a `**` of `c` takes anything, so `c` matches
every path `g` does. `identity.CoveredByAny(globs, g)` and
`identity.FirstUncovered(held, requested)` (the first requested glob no held
glob covers, and `true`) apply it to lists, a `nil` list covering
everything. Matching and covering are one pass over the subject per element
of the glob, never exponential.

`internal/identity/token.go` holds minting, the one way a PAT is created.
`identity.MintPAT(ctx, tx, caller, req, now)` takes the caller's
`identity.Principal`, an `identity.MintRequest{Name, Permissions, Paths,
Expires *int, ExpiresAt *time.Time, Workspace}` and the clock's `now`, and
writes through the transaction it is given without committing, so a caller
can mint a token together with changes of its own. It validates the values
(`Name` 1 to 255 characters, `Permissions` at least one registered scope,
duplicates dropped and the order kept, `Paths` through `ValidatePaths`,
`Expires` 0, 30, 60 or 90 with `nil` meaning 90), each failure a
`400 invalid_request` that stores nothing, then applies the subset rule
(`CheckSubset`, below), then stores the token with
`StorePAT` for `caller.User`, created at `now`. `expires_at` is `Expires` ×
24 hours after `now` truncated to whole seconds, or `nil` for 0, so the
instant the API shows is the instant the token stops authenticating; a
hub-internal caller sets `ExpiresAt` instead, kept to whole seconds.
`Workspace` is `""` on `POST /user/tokens`; binding a token is the
workspace-token minting component's, which calls `MintPAT` with it.
`identity.PAT` marshals to the REST API's Token object (`token_id`,
`user_id`, `name`, `permissions`, `paths`, `workspace`, `expires_at`,
`revoked_at`, `created_at`, `null` for what is unset), which never holds
the token's value; only the creation answer adds `token`.

`internal/identity/subset.go` holds the subset rule, so that every way of
creating or widening a token applies the same code.
`identity.CheckSubset(caller, permissions, paths)` returns `nil` unless
`caller.Type` is `TypePAT`; for a PAT it returns a `403
insufficient_scope` `*server.Error` naming the first requested permission,
in request order, that is not in `caller.Permissions` (exact membership, no
implication), else, when `caller.Paths` is not `nil`, saying `paths` is
required for a `nil` request or naming the first requested glob
`FirstUncovered` reports. `identity.CheckWidenSubset(caller, added,
targetPaths)` is the rule for a request that adds scopes to an existing
token, whose paths do not change: it returns `nil` when `added` is empty,
then refuses, in the same order, the first added scope not in
`caller.Permissions`, and, when `caller.Paths` is not `nil`, a target
without paths (`paths: this token is limited to paths, so it cannot add
scopes to a token that is not`) or the first of the target's globs
`FirstUncovered` reports. Without the paths half a PAT limited to paths
could give a scope it holds to a token without that limit. Both expect
validated values: validation answers first.

`POST /user/tokens`, in `internal/identity/token_routes.go`, is a create
route with `ShowOnce: []string{"token"}`, so a replay under the same
`Idempotency-Key` answers `200` without the value. It declares the scope
`tokens:manage`, which the scope check holds a PAT to before the body is
read, and carries `identity.MarkNeedsUser`. Its handler decodes the body
into a `map[string]json.RawMessage` and checks only each member's JSON type
(`name` a string, `permissions` and `paths` arrays of strings, `paths` also
`null`, `expires` an integer), leaving the values to `MintPAT`, and runs
`MintPAT` in one `(*store.Store).Tx` with `Deps.Now()`.

`token.go` also holds the reads and revocations of PATs.
`identity.RevokeTokenByID(ctx, tx, tokenID, now)` sets `revoked_at` to
`now` when it is `NULL`, so a revoked token keeps its first `revoked_at`,
and `identity.SetTokenExpiryByID(ctx, tx, tokenID, at)` sets `expires_at`
to `at` kept to whole seconds and nothing else. Both write through the
transaction they are given without committing, check no owner, write no
event and return `ErrNotFound` for an unknown id: they are for hub-internal
callers (the hub credential of a sandbox), which change a token together
with changes of their own. Keys and tokens share one lookup by owner and
one list, in `internal/identity/owned.go`: `ownedTable[T]` names a
credential table (`keyTable` for `api_keys`, `tokenTable` for `pats`), its
`ofUser` answers a missing credential and another user's alike with the
table's `404` (`API key not found`, `token not found`), and its `writePage`
answers a page of one user's credentials in any state, newest first
(`created_at`, then the id, both descending) with keyset paging on those
two. The token routes look a token up with `tokenTable.ofUser` and list
with `tokenTable.writePage`; the admin routes call the same functions with
the selected user's id. `GET /user/tokens` and `GET /user/tokens/:token_id` declare
`tokens:read`, `DELETE /user/tokens/:token_id` declares `tokens:manage`
and revokes through `RevokeTokenByID` after the owner lookup, in one
transaction; every `/user/tokens` route carries `identity.MarkNeedsUser`,
so the admin token is refused by authentication and no handler refuses it
again.

`PUT`, `PATCH` and `DELETE /user/tokens/:token_id/permissions` share one
handler, `changeOwnTokenPermissions`, declaring `tokens:write`. In one
`(*store.Store).Tx` it looks the token up with `tokenTable.ofUser` (404), reads
the body's `permissions` with `permissionsChangeOf` (400; registered scopes
for `PUT` and `PATCH`, at least one entry for `PATCH` and `DELETE`),
computes the new list with `changedPermissions` (`PUT` the list without
duplicates, `PATCH` the current list with the new scopes appended, `DELETE`
the current list without the given ones), passes the scopes the new list
adds to the token's current ones and the token's paths to
`CheckWidenSubset` (403), refuses a
revoked or expired token with `409 conflict`, and writes with
`setTokenPermissions`, which stores `[]` and sets `revoked_at` in the same
`UPDATE` when the list is empty. Expiry is decided by `expired(expiresAt,
now)` in `authenticate.go`, the predicate authentication's `checkState`
uses, so a token is `409` here exactly when it is `401` there. `paths`,
`workspace`, `name` and `expires_at` are never written.

`internal/identity/key_routes.go` holds the routes of the caller's own API
keys, each carrying `identity.MarkNeedsUser`. `APIKey.MarshalJSON` writes
the Key object (`key_id`, `user_id`, `expires_at`, `revoked_at`,
`created_at`), so no answer built from a record can carry a secret.
`GET /user/keys` declares `keys:read` and lists with `keyTable.writePage`,
newest first (`created_at`, then `key_id`, both descending). `DELETE /user/keys/:key_id` declares `keys:manage` and revokes
with `revokeKeyOfUser`, which sets `revoked_at` only while it is `NULL`, in
one transaction with the owner lookup. `POST /user/keys/:key_id/refresh`
declares no scope and is not a create route: its handler refuses any caller
but an API key with `403 forbidden` before the lookup, refuses a revoked or
expired key (the same `expired` predicate) with `409 conflict`, and in one
transaction stores the hash of a new `randomString(apiKeyAlphabet,
secretLength)` secret and the new `expires_at` with `setKeySecret`;
authentication reads the stored hash on every request, so the old secret
fails from the next one. Every `:key_id` route resolves the id with
`keyTable.ofUser`, which answers a missing key and another user's alike, `404`
`API key not found`. `s.writeKeys(w, r, userID)` and `s.revokeKey(w, r,
userID)` take the user id, so the admin routes reuse them.

`internal/identity/admin_credential_routes.go` holds the admin routes of
any user's keys and tokens. They carry the admin mark and declare no
scope, so the admin check refuses every PAT. Each handler is
`forSelectedUser` around `writeKeys`, `writeTokens`, `revokeKey` or
`revokeToken`: it resolves the path's `:id` with `UserBySelector`
(`404 user not found`) and passes the user's id on, so an admin's list and
revocation run the same code, and give the same answers, as the user's own.

`POST /users` (`createUser`) decodes the body into a
`map[string]json.RawMessage`, so that a missing member, `null` and a value
of another type are told apart: `username`, `email`, `provider` and
`provider_id` must be non-empty JSON strings and `full_name`, when present,
a JSON string, else `400 invalid_request`. It then calls
`identity.CreateUserAndPersonalOrg` with the role `user` in one
`(*store.Store).Tx`, with `CreatedAt` from `Deps.Now`, answers its
`ErrConflict` `409 conflict` with the error's text, and answers `201` with
the User object. It never promotes: the first-admin rule is the login's.

`identity.UserBySelector(ctx, q, sel)` in `internal/identity/users.go`
resolves a user selector by its shape alone: a value of 36 characters that
the standard library's `uuid.Parse` accepts is looked up as the `id`, any
other value with an `@` as the `email`, and every other value as the
`username`, each with SQLite's default binary comparison, so exactly. It
returns `identity.ErrNotFound` without trying another column, and the
handlers answer that `404 not_found` with the message `user not found`.
Its third case is `identity.UserByUsername(ctx, q, username)`, which
compares the `username` alone, whatever the value's shape; the landing
policy looks its `user:` approvers up with it, so that a document never
names a user by id or e-mail.
`GET /users/:id` reads through the store's `*sql.DB`; `PATCH /users/:id`
reads the body first, with the rule of `PATCH /user` (`fullNameOf` in
`handlers_users.go`), then resolves the selector and calls `updateFullName`
in the same `(*store.Store).Tx`. `GET /users` reads one page with
`listUsers(ctx, q, includeBlocked, after, limit)` in `users.go`: `status =
'active'` unless the query parameter `include_blocked` is exactly `true`,
`ORDER BY created_at DESC, id DESC`, continuing after the position
`{created_at in nanoseconds, id}` that the cursor of `server.ReadPage`
carries, and writes it with `server.WritePage`.

Promote, demote, block and unblock (`setRole` and `setStatus` in
`handlers_users.go`) go through `changeUser`, which resolves the selector
and applies the change in one `(*store.Store).Tx`. A user who already has
the requested role or status is answered as read, without a write, so
`updated_at` stays; otherwise `updateRole` or `updateStatus` in `users.go`
sets the column and `updated_at` (from `Deps.Now`). No credential is
touched: authentication refuses a blocked user's credentials on its own.
The last-admin guard is `guardLastAdmin(ctx, tx, u)` in `users.go`: it
returns the conflict `the user is the only active admin` when `u` has the
role `admin` and the status `active` and no other user has both, and nil
otherwise. Demote calls it before `updateRole`, in the same transaction
that read `u`; since the store admits one writer at a time, the count and
the change cannot interleave with another demotion or deletion. Block has
no guard.

`DELETE /users/:id` (`deleteUser` in `handlers_users.go`) is an admin route.
In one `(*store.Store).Tx` it resolves the selector (`errUserNotFound()`
before anything else), calls `guardLastAdmin` (`409 conflict`, before any
remover), calls the user removers through `removeUserData` in
`removers.go`, then `deleteUser(ctx, tx, id, now)` in `users.go`. That
function deletes the user's `api_keys` and `pats`, bound ones included, and
`memberships`, runs `UPDATE orgs SET owner_id = NULL, updated_at = ? WHERE
owner_id = ?` and deletes the user, each an explicit statement rather than
left to the foreign keys' `ON DELETE` actions, so that an org's
`updated_at` moves with its `owner_id`. The orgs are kept, so a user delete
calls no org remover. It answers `204`.

`GET /user` and `PATCH /user`, in `internal/identity/handlers_user.go`,
carry the mark `identity.MarkNeedsUser` and declare the scopes `users:read`
and `users:write`. Both take the caller's user from the `identity.Principal`
of the request. `PATCH /user` reads the body with `fullNameOf(r)`, the rule
`PATCH /users/:id` shares: a `map[string]json.RawMessage` that needs
`full_name` as a JSON string, else `400 invalid_request`; it stores it in one `(*store.Store).Tx` through
`updateFullName(ctx, tx, id, fullName, now)` in
`internal/identity/users.go`, which sets `full_name` and `updated_at` (from
`Deps.Now`) and returns the stored user, or `identity.ErrNotFound`.

`internal/identity/orgs.go` holds the org reads: `identity.Org` marshals to
the REST API's Org object (`id`, `name`, `slug`, `url`, `owner_id`, `null`
when `OwnerID` is empty, `status`, `created_at`, `updated_at`);
`identity.OrgByID(ctx, q, id)` and `identity.OrgBySlug(ctx, q, slug)`
return an org or `identity.ErrNotFound`, comparing the slug exactly;
`identity.PersonalOrg(ctx, q, userID)` returns the user's personal org, the
one owned by the user and marked `personal` by `CreateUserAndPersonalOrg`,
whatever the username and the org's name are now, or `identity.ErrNotFound`;
and
`identity.Memberships(ctx, q, orgID)` returns an org's
`identity.Membership`s (`OrgID`, `UserID`, `CreatedAt`), oldest first, then
by user id; `identity.ActiveOrgIDs(ctx, q, userID)` returns, by id, the
ids of the orgs with status `active` of which a user is a member, the
membership source of the workspace access rule (see Workspace access). Both
read through an `identity.RowsQuerier`, an `identity.Querier` that also has
`QueryContext` (the store's `*sql.DB` or a `*sql.Tx`).

`identity.OrgBySelector(ctx, q, sel)` in `orgs.go` resolves an org selector
by its shape alone, as `UserBySelector` does a user's: a value `isUUID`
accepts is looked up as the `id`, every other value as the `slug`, exactly,
and `identity.ErrNotFound` is returned without a second lookup. The org
routes are `orgRoutes()` in `internal/identity/handlers_orgs.go`, which
`Routes()` appends. `POST /orgs`, `GET /orgs` and `PATCH /orgs/:id` are admin
routes. `POST /orgs` (`createOrg`) decodes the body into raw members, trims
the name (`orgNameMember`), checks the slug against `^[a-z0-9](?:[a-z0-9_-]{0,126}[a-z0-9])?$`
(`validSlug`), and in one `(*store.Store).Tx` reads the `owner_id` with
`UserByID` (not found is `400`) and calls `createOrg(ctx, tx, o)`, which
answers a taken name or slug with a conflict and inserts the org with a
new UUID, `owner_id` NULL when there is none, and no membership.
`PATCH /orgs/:id` checks the body first, then resolves the selector and
calls `updateOrg(ctx, tx, id, name, url, now)`, which refuses a name another
org has (`id <> ?`) and sets only the members given and `updated_at`.
`GET /orgs` reads one page with `listOrgs(ctx, q, includeBlocked, "", after,
limit)`: `ORDER BY name`, continuing after the position `{name}` of the
cursor. `GET /orgs/:id` carries no admin mark and declares the scope
`orgs:read`; it goes through `visibleOrg(r)`, the one rule for reading an
org: an admin (`isAdmin`) reads every org, any other caller an active org
of which its user is a member (`isMember`), and every other case, a missing
org included, is the one answer `errOrgNotFound()`, `404 not_found` with the
message `org not found`.

`GET /orgs/:id/members` (`listMembers`) also carries no admin mark and
declares `orgs:read`. It reads the page (`server.ReadPage` and `pageAfter`)
before it looks the org up, so a bad `limit` or `cursor` is `400
invalid_request` for every caller and tells nothing of the org; then it goes
through `visibleOrg(r)`, so a blocked org's member list is an admin's alone.
It reads one page with `listMembers(ctx, q, orgID,
after, limit)` in `orgs.go`, a join of `memberships` and `users` `ORDER BY
username`, continuing after the position `{username}`; an item is a
`memberItem` (`org_id`, `user_id`, `username`, `email`, and the membership's
`created_at`). `PUT` and `DELETE /orgs/:id/members/:user_id` (`addMember`,
`removeMember`) resolve, in one `(*store.Store).Tx`, the org selector and
then the user selector (`orgAndUser`: `errOrgNotFound()` or
`errUserNotFound()`), then call `addMembership(ctx, tx, orgID, userID, now)`,
an `INSERT … ON CONFLICT DO NOTHING` that leaves an existing membership as
it is, or `removeMembership(ctx, tx, orgID, userID)`, which returns
`identity.ErrNotFound` when no row was deleted (`404 not_found`, `membership
not found`) and leaves `owner_id` alone; both answer `204`.
`GET /user/orgs` (`listOwnOrgs` in `handlers_user.go`) carries the
needs-a-user mark and declares `orgs:read`; it reads the active orgs of
which the caller is a member through `listOrgs(ctx, q, includeBlocked,
memberID, after, limit)`, the reader of `GET /orgs`, whose `memberID` limits
the list to that user's orgs. `pageAfter[T](page)` decodes a page's
position for these lists and for `GET /users`.

`POST /orgs/:id/block` and `/unblock` (`setOrgStatus` in
`handlers_orgs.go`) are admin routes: in one `(*store.Store).Tx` they
resolve the selector and, unless the org has the requested status already,
call `updateOrgStatus(ctx, tx, id, status, now)` in `orgs.go`, which sets
`status` and `updated_at` and leaves the memberships alone; an org that has
the status is answered as read, so `updated_at` stays. A blocked org leaves
`GET /user/orgs` and the default `GET /orgs` because both lists read
`status = 'active'`, and `visibleOrg` answers it to admins only.
`GET /orgs/:id` and `GET /orgs/:id/members` both go through `visibleOrg`, so
a missing org, an org of which the caller is no member and a blocked org of
which it is a member get one and the same `errOrgNotFound()`.

`DELETE /orgs/:id` (`deleteOrg` in `handlers_orgs.go`) is an admin route.
In one `(*store.Store).Tx` it resolves the selector (`errOrgNotFound()`
before anything else), calls the org removers, then `deleteOrg(ctx, tx,
id)` in `orgs.go`, which deletes the org's memberships and the org; users
are never touched, so the owner and the members stay. A remover is an
`identity.Remover`, `func(ctx context.Context, tx *sql.Tx, id string)
error`, registered on the service with
`(*identity.Service).RegisterOrgRemover` or `RegisterUserRemover` in
`internal/identity/removers.go`, and kept in registration order under a
mutex ([ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)).
`removeOrgData` and `removeUserData` call the org or the user removers,
a copy of the list taken under the mutex, in order with the delete's
transaction and the id and stop at the first error, which
`callRemovers` returns as text only, not wrapped, so that whatever a
remover returns the delete answers `500 internal` and the transaction rolls
back, with everything earlier removers wrote through it. A package that
keeps data for a user or an org registers its remover from its part in
`internal/hub` through the `identity` field of `hub.Composition`, which
`composeIdentity` sets.

### Workspace access

`internal/hub/access.go` adds the part `composeAccess`, after
`composeIdentity` in `parts`. It adds the schema steps `access.SchemaStep`
(`workspace_records`) and, after it, `access.CreatingTokenStep`
(`workspace_creating_token`), registers a zero `access.Service`, installs
its `Guard` as the pipeline's binding guard (`SetBindingGuard`), adds its
`SubsetCheck` and then its `Check` to the access checks, after identity's
admin check, registers its `TokenRoutes()`, registers its
`SweepBindings` as the maintenance task `token_binding_sweep`, and binds it in
the start hook `access` to `access.Deps{Records:
access.StoreRecords(rt.Store.Main()), Memberships: ..., Refusals:
access.NewRefusalLimiter(events, rt.Now, rt.Logger), Events: events, Store:
rt.Store, Now: rt.Now, Logger: rt.Logger}`, the
membership source being `identity.ActiveOrgIDs` over the same store and
`events` the adapter `accessEvents`, which implements
`access.EventEmitter` over the event component: it converts an
`access.Event` to an `event.Input` (an empty `Workspace` or `ActorID`
becomes nil, `Metadata` is passed as is) and calls `c.Events().Emit` or
`EmitNow`, reading the component from the composition at each emit, since
`composeEvents` comes after `composeAccess`. The limiter is built in the
hook, so every start of the hub begins with no refusal counted. The same hook
installs the covered set of workspace-bound tokens in the event component,
`c.Events().SetCoverage(...)`: an `event.CoverageFunc` that answers whether
`(*access.Service).CoveredSet` of the caller contains the slug, with the
`event.Caller`'s `CredentialID` (the PAT's token id) as `TokenID` and its
`Workspace` as the binding. The hook runs before the `events` hook, whose
bind keeps what `SetCoverage` installed. It adds the route
rule `refusePublicNeed`: a route that is `Public` and declares
`access.MarkNeed` stops the start with `compose the routes: <method>
<pattern>: a public route cannot declare the workspace need access.need:
the pipeline runs no access check on it`, because the pipeline skips the
access checks on a public route and its handler would serve every caller
without a decision. It registers no
remover on the identity service: a workspace record outlives the deletion of
its owner and of its org.

The step creates the table `workspaces` in the transactional store:

| Column | Holds |
|---|---|
| `slug` | the workspace's slug, the primary key, compared byte for byte (no case folding) |
| `owner_id` | the id of the user who owns it; no foreign key to `users` |
| `org_id` | the id of its org; no foreign key to `orgs` |
| `kind` | `repository` or `fork` (a `CHECK` constraint) |
| `parent_slug` | the slug of the repository a fork was created from; NULL for a repository, and no foreign key |
| `status` | `pending`, `cloning`, `ready`, `failed` or `archived` (a `CHECK` constraint) |
| `creating_token_id` | the `token_id` of the PAT whose request created a fork; NULL for a repository and for a fork no token created, and no foreign key. Added by the second step, `ALTER TABLE workspaces ADD COLUMN creating_token_id TEXT`, so that a store whose table exists gains it |

with an index on each of `owner_id`, `org_id` and `parent_slug`, for the
list reads. A dangling `owner_id` matches no caller and a dangling `org_id`
grants no membership, because the memberships of a deleted org are deleted
with it.

`internal/access/record.go` holds the record type `access.Record` (`Slug`,
`OwnerID`, `OrgID`, `Kind`, `ParentSlug`, `Status`, `CreatingTokenID`;
`ParentSlug` and `CreatingTokenID` are `""` for a repository and stored as
NULL, and `CreatingTokenID` has no JSON name, so no answer shows it), the constants `KindRepository`,
`KindFork` and `StatusPending` to `StatusArchived`, and the store functions.
The insert takes the caller's `*sql.Tx`; the reads take a `Querier` (the
store's `*sql.DB` or an open `*sql.Tx`):

- `InsertRecord(ctx, tx, rec)` stores a record in the caller's transaction.
  It refuses, with an error wrapping `access.ErrInvalidRecord` and without
  writing, a record with an empty slug, owner or org, an unknown kind or
  status, a repository with a parent or a creating token, a fork without a
  parent, and a fork whose
  parent has no record, is a fork itself or is in another org; the parent is
  read in the same transaction. A slug that has a record already is
  refused with `access.ErrDuplicateSlug` (the insert is `ON CONFLICT (slug)
  DO NOTHING`), and the stored record stays as it was. Fork creation records
  the creating token through it, in `Record.CreatingTokenID`.
- `RecordBySlug(ctx, q, slug)` returns the record and `true`, or `false`
  and a nil error for any string no record has as its slug.
- `ForkParent(ctx, q, slug)` returns the parent's slug when `slug` names a
  task fork, and `""` for a repository or a slug no record has.
- `RecordsOwnedOrIn(ctx, q, ownerID, orgIDs)` lists the records owned by a
  user or in one of a list of orgs, and `ForksOf(ctx, q, parents)` the forks
  of a list of parents, both ordered by slug; an empty list of parents
  lists nothing.

The rule reads records through the interface `access.RecordReader`
(`Record`, `OwnedOrIn`, `ForksOf`), so that a test can make a read fail;
`access.StoreRecords(q)` is the one over these functions.

#### Levels

`internal/access/level.go` resolves what a caller is on one workspace,
`access.Level`: `LevelAdmin` (`"admin"`), `LevelOwner` (`"owner"`),
`LevelMember` (`"member"`) or `LevelNone` (`""`). The caller is
`access.Caller{Admin, UserID, TokenID, Workspace}`, built from the
pipeline's `server.Caller` by `access.CallerFrom`. `TokenID` and `Workspace`
are set only for a PAT bound to a workspace, from the `identity.Principal`
in the caller's `Detail` (`Type` `pat` with a `Workspace`): the token's id
and the bound slug. `Level` and `Decide` do not read them. The kind of credential and the user's role are not
part of it, so a user has the same level by API key as by PAT, and a user
with the role admin has no level from the role.

`(*Service).Level(ctx, caller, slug)` returns the level, the record and
whether the slug has one. It reads the record first: a slug with no record
is `LevelNone` and found `false`, for every caller. For a user, though, a
missing slug still reads the user's active orgs, as a repository the user
cannot see would, and returns that read's error if it fails. A missing slug
therefore costs the same reads as an invisible repository, and fails the
same way. An invisible fork costs one record read more (its parent), and
that difference is not matched. Then:

- the admin token is `LevelAdmin` on every workspace, repository or fork;
- on a repository, a user whose id is the record's `owner_id` is
  `LevelOwner`, decided before any membership is read; otherwise a user who
  is a member of the record's `org_id`, as the membership source answers
  it, is `LevelMember`; everyone else is `LevelNone`;
- on a fork, the user's level on the parent record is resolved first, as on
  a repository. `LevelNone` on the parent, a parent with no record, or a
  parent that is not a repository gives `LevelNone` on the fork, even to the
  fork's owner: a fork never grants more than its parent. Otherwise the
  fork's owner and the parent's owner are `LevelOwner` on the fork, and
  every other user `LevelMember`.

The workspace's status is never read, so a caller has the same level in
every status. A failed read of a record or of the memberships returns
`LevelNone`, no record and the error, never a grant.

The membership source is the narrow interface `access.Memberships`, one
method `ActiveOrgIDs(ctx, userID)`; `access.MembershipsFunc` adapts a
function to it. The hub wires `identity.ActiveOrgIDs(ctx, q, userID)` from
`internal/identity/orgs.go`, which joins `memberships` with `orgs` and
returns, by id, the ids of the orgs with status `active` the user is a
member of. A blocked org is left out, so its members are `LevelNone` on its
workspaces while it is blocked (its owner is not), and a deleted org has no
memberships left, its rows being deleted with it.

#### Decision and check

`internal/access/decide.go` turns a level into an answer for what a route
needs, `access.Need`: `NeedRead` (`"read"`), `NeedWrite` (`"write"`) or
`NeedOwner` (`"owner"`). `(*Service).Decide(ctx, caller, slug, need)`
returns an `access.Decision{Outcome, Level, Record}`, a comparable value:

- level none, or a slug with no record: `Outcome` `access.NotFound`
  (`"not_found"`) with `LevelNone` and the zero record, for every need, so
  the decision for a workspace the caller cannot read equals the one for a
  slug with no workspace;
- `LevelMember` with `NeedWrite` or `NeedOwner`: `access.ReadOnly`
  (`"read_only"`) with the level and the record;
- everything else: `access.Granted` (`"granted"`) with the level and the
  record.

A need other than the three, and a failed read, return the zero decision
and an error, never a grant. The slug's shape is not checked: a string that
cannot be a slug has no record. A surface other than the REST API, such as
the git server, calls `Decide` directly.

`internal/access/middleware.go` holds the REST side. A workspace route
declares its need in its `Marks` under `access.MarkNeed`
(`"access.need"`), with the value `read`, `write` or `owner`, and names its
workspace with the path parameter `slug`. `(*Service).Check` is a
`server.Check` in the access position: on a route without the mark it
passes; otherwise it takes the caller from `server.CallerOf` through
`access.CallerFrom` and the slug from the matched route, and decides. A
grant attaches the decision with `server.Attach`, and the handler reads it
with `access.DecisionOf(r)`; `ReadOnly` is answered
`server.Forbidden("org members have read-only access to this workspace")`
and `NotFound` `server.NotFound("workspace not found")`, built from
constants only, so its bytes cannot differ between an invisible workspace
and a missing one. A caller that is neither the admin token nor a user, a
mark other than the three needs, a marked route without a `slug`
parameter, and an error from `Decide` are returned as plain errors, which
the pipeline logs and answers `500 internal` with `internal error`. No
route declares the mark yet.

The check never reads the request's method: a route that needs `read`
grants a member with `POST`, `PATCH`, `PUT` or `DELETE` exactly as with
`GET`, because a member's own contributions (forks, changes, verdicts,
claims) change state on routes that need `read`, and those routes check
their own conditions. The decision `access.DecisionOf(r)` returns holds the
caller's level and all six fields of the record as read while deciding, so
such a condition (the fork's owner, its parent, its kind) needs no second
lookup.

`(*Service).CanWrite(ctx, userID, slug)` answers whether a named user, who
need not be the caller, has write access to a workspace: `true` exactly
when that user's level on it, resolved by `Level` as for a caller with that
`UserID`, is `LevelOwner`; `false` for `LevelMember`, `LevelNone` and a slug
with no record; `false` and the error when a read fails. No production code
calls it yet; the routes whose conditions ask about another user, such as a
change's author, will.

#### Readable set

`internal/access/readable.go` answers which workspaces a caller can read,
for the reads that span workspaces (the workspace list, events, audit), so
that they show exactly the workspaces the per-workspace decision lets
through. `(*Service).Readable(ctx, caller)` returns an
`access.ReadableSet{All, Entries}`:

- the admin token gets `All` set and no entries; `Contains` answers yes and
  `Level` `LevelAdmin` for every slug;
- a user gets `Entries`, the user's level by slug. It reads the user's
  active orgs (`Memberships.ActiveOrgIDs`), then the records the user owns
  or that are in one of those orgs (`RecordReader.OwnedOrIn`, which brings
  the forks of those orgs' repositories with them, a fork being in its
  parent's org), then the forks of the repositories the user owns
  (`RecordReader.ForksOf`). Each record, in every status, gets the level
  the resolver of `Level` gives it, and those at `LevelNone` are left out:
  a fork whose owner has no level on its parent, a workspace of a blocked
  org that the user does not own. A slug appears once; an owned workspace
  in one of the user's orgs is `LevelOwner`;
- a caller that is neither, and a user with nothing, get the empty set and
  no error;
- a PAT bound to a workspace gets its user's set cut down to the covered
  set of its binding (below), each entry at the user's level: a covered
  workspace the user cannot read stays out;
- a failed read returns the error and the empty set, never part of one.

`Level` and `Readable` share one resolver (`userLevels` in `level.go`); they
differ only in where the user's active orgs and a fork's parent come from.
`Level` reads them from the sources as the answer needs them; `Readable`
takes the orgs it has read and looks a parent up among the records it has
collected, which hold every repository the user has a level on, so a parent
that is not among them is one the user has no level on. A workspace is
therefore in a user's set exactly when `Level` resolves a level other than
`LevelNone` for it, with that level. The visibility decision of the hub
events calls `Readable` (see [Hub events](#hub-events)), and so does the
workspace list (`GET /workspaces`, see Workspace records); the audit query
will.

#### Workspace-bound tokens

`internal/access/coverage.go` says what the binding of a PAT bound to a
workspace reaches. `(*Service).CoveredSet(ctx, caller)` returns, sorted, the
covered workspaces: for a token bound to a repository W, W and every fork of
W whose `creating_token_id` is the token's id; for a token bound to a fork,
the fork alone; for a bound slug with no record, that slug alone. A caller
without a binding covers nothing (`nil`). `(*Service).RelationOf(ctx,
caller, slug)` answers how a workspace a request names stands to the
binding, as an `access.Relation` (`internal/access/binding.go`):
`RelationBound` (`"bound"`) for the bound slug, record or not;
`RelationCreatedFork` (`"created_fork"`) for a fork of it the token created;
`RelationParentOfBoundFork` (`"parent_of_bound_fork"`) for the parent of the
fork the token is bound to; and `""` (none) for every other slug and every
caller without a binding. Both read the records on every call, so a fork the
token has just created is covered at once. `access.Binding{TokenID,
Workspace, Relation}` is the value a binding guard attaches to a request
for its handler (ADR 04), read with `access.BindingOf(r)`; it reports false
for every caller but a bound PAT.

`internal/access/guard.go` holds the binding guard, `(*Service).Guard`, a
`server.Check` for the binding-guard position of the pipeline, after the
route's feature check and before the scope check; `composeAccess` installs
it. It runs only on a route that matched, for a caller that authenticated,
since routing and authentication answer first, and never on a public route.
It does nothing for the admin token, an API key and a PAT that is not
bound. A route names a workspace when its pattern is `/workspaces/:slug` or
lies under it; the slug is the path value as typed. On a route that names
no workspace a bound PAT passes when the method and the pattern, relative to
`/api/v1`, are in the allowlist of `internal/access/allowlist.go`, with the
`Binding` of the bound slug and no relation attached, from which the
route's handler reads the covered set it confines its result to
(`CoveredSet(ctx, access.Caller{TokenID: b.TokenID, Workspace:
b.Workspace})`); every other such route, every identity and admin route
among them, is answered `403 token_bound` with the message `a token bound
to a workspace cannot use this route`, before the scope check, whatever
scopes the token holds. The allowlist is `GET /workspaces`, `GET /events`,
`GET /events/log`, `GET /audit`, `GET /sandbox-profiles`, `POST` and `GET
/sessions`, `GET /sessions/:id`, `POST /sessions/:id/complete`, and `POST`
and `GET /sessions/:id/usage`; like the contributor table it matches the
pattern as registered, so a route reaches a bound token only when it is
registered with these parameter names. On a route that names a workspace,
for a bound PAT the guard asks `RelationOf` and:

- for `bound` and `created_fork`, attaches the `Binding` and passes;
- for `parent_of_bound_fork`, attaches the `Binding` and passes when the
  method and the pattern relative to `/workspaces/:slug` are in the
  contributor table, and answers `403 token_bound` otherwise;
- for none, answers `404 not_found` with the message `workspace not found`,
  the bytes the access check answers for a workspace the caller cannot read,
  for an existing and an unknown slug alike, and before the scope check, so
  a token without the route's scope gets the same `404`.

It makes no access or scope decision: a request it passes reaches the scope
check and the access checks as an unbound PAT of the same user would. Each
`403` and `404` it answers is reported to `Deps.Refusals`, an
`access.RefusalReporter` (`Refused(ctx, access.Refusal)`; nil discards),
as `access.Refusal{TokenID, UserID, BoundWorkspace, RequestedWorkspace,
Method, Route, Status, Surface}`, where `Route` is `/api/v1` followed by the
matched pattern, `RequestedWorkspace` is nil on a route that names no
workspace, and `Surface` is `access.SurfaceAPI` (`"api"`). A failed read of
the records is an error, answered `500 internal`.

`(*Service).GuardGit(ctx, caller, slug, method, pattern)` is the same guard
for the git server, which calls it after authentication with the
`server.Caller`, the slug of the repository, the method and the matched git
route's pattern, and returns an `access.GitDecision{Pass, Status, Binding}`.
A bound PAT passes with its `*Binding` on the bound workspace and on a fork
the token created; on every other slug, the parent of the bound fork
included, `Pass` is false and `Status` `404`, and the refusal is reported
with `Surface` `access.SurfaceGit` (`"git"`), `Route` the git pattern as
given and `RequestedWorkspace` the slug. The admin token, an API key and an
unbound PAT pass with a nil `Binding`. A failed read is a zero decision and
the error. No git server calls it yet.

`internal/access/contributor_routes.go` holds the contributor table, the 25
routes a token bound to a fork reaches on the fork's parent, as method and
pattern relative to `/workspaces/:slug`, unexported and read by the guard
alone: `POST` and `GET /changes`, `GET` and `PATCH
/changes/:number`, `GET /changes/by-commit/:sha`, `POST
/changes/:number/abandon`, `/restore`, `/restack` and `/land`, `GET
/changes/:number/versions`, `/versions/:version`, `/diff`, `/stack`,
`/lands`, `/conflict`, `/conflict/file`, `/approvals`, `/checks` and
`/overlaps`, `GET /lands/:id`, `GET /policy`, `POST /policy/evaluate`, `GET
/activity`, `GET /claims` and `GET /claims/:id`. The guard matches the
pattern as registered, so a route reaches a fork-bound token on the parent
only when it is registered with these parameter names.

`internal/access/events.go` declares `access.EventEmitter` (`Emit(ctx, tx,
access.Event)` in the caller's transaction, `EmitNow(ctx, access.Event)` in
one of its own) over the value `access.Event{Type, Workspace, ActorType,
ActorID, ResourceType, ResourceID, Metadata}`. The event component imports
`internal/access`, so this package cannot name `event.Input`, and the hub
passes the event component behind the interface (`accessEvents` in
`internal/hub/access.go`). There is no discarding default.

`internal/access/mint.go` holds the one minting component of bound tokens,
`(*Service).MintBound(ctx, tx, access.MintInput{Creator, Workspace, Name,
Permissions, Paths, Expires, ExpiresAt})`, which returns the
`identity.PAT` and its value. In the caller's transaction, which it does not
commit, it mints through `identity.MintPAT` with `Workspace` set to the
slug, so the token is owned by `Creator.User`, validated as for `POST
/user/tokens` and, for a PAT creator, held to the subset rule, and then
emits `access.BindEventType` (`parley.token.bind`) through `Deps.Events` in
the same transaction: workspace the bound slug, actor type the creator's
`identity.Principal.Type` (`api_key` or `pat`) and actor the owner,
resource `token` and the token id, and metadata `token_id`, `workspace`,
`permissions`, `paths` (or null) and `expires_at` (or null, in the Token
object's time format), never the value. `identity.MintPAT`'s errors are
returned unchanged (`400 invalid_request`, `403 insufficient_scope`), with
nothing written; a failure of the emitter is returned wrapped, and the
caller rolls back the token with its transaction. A service without an
emitter refuses to mint. `ExpiresAt` is the instant a hub-internal caller
(fork creation, the hub credential of a sandbox) sets in place of
`Expires`. No production caller besides the route below uses it yet.

`internal/access/bound_tokens.go` holds `(*Service).TokenRoutes()`, which
`composeAccess` registers: `POST /workspaces/:slug/tokens` with `Scope`
`tokens:manage`, `Create`, `ShowOnce` `token` and the marks
`access.MarkNeed` `owner`, `access.MarkSubsetRule` and
`access.MarkRefusesBound`, and `GET /workspaces/:slug/tokens` with `Scope`
`tokens:read` and the marks `access.MarkNeed` `owner` and
`access.MarkRefusesBound`. On a route marked `access.MarkRefusesBound`
(`"access.refuses_bound"`) the binding guard answers a bound PAT `403
token_bound` on every workspace whose relation to the binding is not none
(bound, created fork, parent of the bound fork), and on a route that names
no workspace even when it is allowlisted; outside the binding the `404`
comes first. The list handler reads the page (`server.ReadPage`), then, in
one read-only transaction, the ids of the PATs whose `workspace` is the
slug in any state, ordered by the whole second of `created_at` (stored in
nanoseconds) and then `token_id` after the cursor's position, which holds
that second and the id, and each PAT with `identity.PATByID`, and answers them
with `server.WritePage`; the admin token passes, since the access rule
gives it owner, and an archived workspace is listed. On a route marked
`access.MarkSubsetRule` (`"access.subset_rule"`) the access check
`(*Service).SubsetCheck`, installed before `Check`,
decodes the body with `identity.DecodeMintRequest` and applies
`identity.CheckSubset` to a PAT caller, so the `403 insufficient_scope` of
the subset rule comes before the workspace is looked up; a body that does
not decode, or whose paths are not valid, passes to the handler. The
handler answers the admin token `403 forbidden` (`this route needs a user`)
before it reads the body, decodes the body (`400`), and then, in one store
transaction, mints with `MintBound`, refusing an archived workspace with
`409 workspace_archived` after the token is validated and before its event
is emitted, so the refusal rolls back the token and emits nothing. Any other
failure of the insert, the event or the commit rolls back and is answered
`500 internal`. The answer is `201` with the Token object followed by
`token`, the value.

`internal/access/revoke.go` holds the revoking component of bound tokens,
`(*Service).RevokeBoundTo(ctx, tx, slugs)`, for the components that remove
workspaces (workspace delete, fork expiry), which call it in the
transaction that removes them, so the removal and the revocation commit or
roll back together. In that transaction, which it does not commit, one
`UPDATE pats ... RETURNING token_id` sets `revoked_at` to the instant of
`Deps.Now` on every PAT whose `workspace` is one of `slugs`, whose
`revoked_at` is NULL and whose `expires_at` is NULL or after that instant,
and it returns their token ids sorted ascending, a non-nil list that is
empty for no slug or no live token. A token already revoked keeps its
`revoked_at` and an expired one is left as it is (unlike
`identity.RevokeTokenByID`, which revokes an expired token too), and
neither is returned. On a failed write it returns the error and a nil
list, and the caller rolls back. It emits no event: the removal's own event
carries the ids. A revoked token is answered `401 credential revoked` by
authentication from the next request on, before the binding guard, so it
is never reported as a refusal. No production caller uses it yet.

`internal/access/sweep.go` holds the token binding sweep,
`(*Service).SweepBindings(ctx)`, which the hub runs as the maintenance task
`token_binding_sweep`, so that a bound token a removal left behind stops
working within one maintenance interval. In one transaction of `Deps.Store`
it reads the distinct `workspace` of the PATs that are bound and not revoked
and have no row in `workspaces`, and revokes their live tokens with
`RevokeBoundTo`, so a token that is unbound, bound to a workspace with a
record in any status (archived included), revoked or expired is left as it
is. It returns how many it revoked and, when that is more than 0, logs one
info line `revoked the tokens bound to workspaces that no longer exist` with
`count` to `Deps.Logger` (nil discards); it emits no event. On a store
failure the transaction is rolled back and it returns 0 and the error, which
the maintenance loop logs as `maintenance task failed`. A context cancelled
before it begins returns the context's error and writes nothing; once
begun, the transaction runs to its end with a context that a stop does not
cancel, because `database/sql` rolls a cancelled transaction back in a
goroutine of its own, which could close the store's last connection after
the hub has closed the store and returned from `Run`.

`internal/access/refusal.go` holds the hub's `RefusalReporter`,
`access.RefusalLimiter`, built with `access.NewRefusalLimiter(events, now,
logger)` (nil `now` is `time.Now`, nil `logger` discards). It keeps, in
memory and under a mutex, for each token id the time of the token's last
event and the count of refusals suppressed since. A refusal of a token
with no event in the preceding 60 seconds is emitted with `EmitNow` as
`access.RefusedEventType` (`parley.token.bound_refused`): workspace the
bound slug, actor type `pat` and actor the token's user, resource `token`
and the token id, and metadata `token_id`, `bound_workspace`, `method`,
`route`, `status`, `surface`, `suppressed` (the count, then reset to 0) and,
only when the request named a workspace, `requested_workspace`, the slug as
typed; the token's minute starts then. A refusal inside the minute emits
nothing and adds 1 to the count. Each refusal first drops every entry
whose minute has passed with a count of 0, so the map holds only the
tokens refused in the last minute or with refusals still to report. The
emit runs after the guard has decided the answer, with the request's
context detached from its cancellation, and cannot change it: an error, or
a panic, which it recovers, is logged once at warn level (`the refusal of
a workspace-bound token was not recorded`, with `token_id`,
`bound_workspace` and `error`, never a token value) and not retried, and
the token's minute counts as started. A limiter without an emitter logs
the same warning.

### Workspace records

`internal/workspace/store.go` holds `workspace.SchemaStep`
(`workspace_fields`), which extends the table `workspaces` of
`access.SchemaStep` instead of keeping a second table, so that the access
rule and the workspace routes read one row; it applies after
`access.CreatingTokenStep`. It adds one column per `ALTER TABLE workspaces
ADD COLUMN` statement: `workspace_mode` (`standard` or `carry_patch`, a
`CHECK` constraint, default `standard`), `git_url`, `default_branch`,
`display_name` and `description` (default `''`), `head_sha`,
`origin_head_sha`, `clone_error`, `last_sync_at`, `sync_error`,
`upstream_url`, `integration_branch`, `upstream_head_sha`, `fork_point_ref`
and `fork_point_sha` (both NULL unless the workspace is a fork),
`expires_at`, `auto_change`, `purpose`, `session_id`, `settings` (the stored
settings document as one JSON object, default `'{}'`), `created_at` and
`updated_at` (default `0`), `instance_id` (default `''`) and `maintenance`
(`''`, `reclone`, `archive` or `delete`, a `CHECK` constraint). Times are
UnixNano integers. The slug stays the primary key, unique across
repositories and forks. Origin heads have no column: they are refs of the
workspace's copy. `composeWorkspaces` registers the step.

`internal/workspace/record.go` holds `workspace.Workspace`, every field of
a workspace (a nil pointer for a null field) plus `InstanceID`, `Marker` and
`CreatingTokenID`, which no response shows; the type `Status` with
`StatusPending` to `StatusArchived` (the access rule's strings); the type
`Marker` with `MarkerNone` (`""`), `MarkerReclone`, `MarkerArchive` and
`MarkerDelete`; `ModeStandard` and `ModeCarryPatch`; `ForkPoint` (`Ref`,
`SHA`); and `NewInstanceID`, 128 random bits as 32 hex digits. The store
functions take the caller's transaction:

- `Insert(ctx, tx, w)` refuses an empty `InstanceID` with
  `access.ErrInvalidRecord` before writing, stores the access rule's columns
  with `access.InsertRecord` (so a taken slug is `access.ErrDuplicateSlug`)
  and then the other fields with an `UPDATE` in the same transaction; a nil
  `Settings` is stored as `{}`.
- `BySlug(ctx, q, slug)` reads the record through an `access.Querier`; an
  unknown slug is found = false with a nil error, and `Settings` is never
  nil.
- `StartClone(ctx, ex, slug, now)` moves a `pending` or `cloning` workspace
  to `cloning` and clears its `clone_error` in one `UPDATE`; it reports
  started = false, with no error, for any other status or no workspace.
- `UpdateClone(ctx, ex, slug, c)` writes `CloneFields` (`Status`,
  `DefaultBranch`, `HeadSHA`, `OriginHeadSHA`, `CloneError`, `UpdatedAt`),
  every one of them, a nil one as NULL, through an `Execer` (a `*sql.Tx` or
  the `*sql.DB`); an unknown slug is `workspace.ErrNotFound`.
- `RecordHead(ctx, ex, slug, id)` writes `head_sha` alone, leaving the
  status and `updated_at` as they are, through an `Execer`; an unknown slug
  is `workspace.ErrNotFound`. The ref writer's head_sha setter calls it
  after a write moved a repository's default branch.

`internal/hub/workspace.go` adds the part `composeWorkspaces`, after
`composeSecrets`. It creates the `*workspace.Service`, registers
`workspace.SchemaStep` and the workspace routes, and adds the start hook
`workspaces`, which builds the service's `workspace.Deps` (the stores, the
secret and event components through the `Secrets` and `Events` interfaces,
the access part's `*access.Service` as `Access`, an `AccessRule` that gives
the workspace list the caller's readable set, the hub's scrubber, logger and clock, `[server].external_url` as
`ExternalURL` and `[workspace].path` as `Root`, and `Lister`, an
`OriginLister` that lists an origin's refs with a `Credential`, the type
`gitrepo.Credential`, so that a resolved credential goes to git unchanged),
applies every function `AdjustWorkspace` added, and binds the service with
`Bind`. First of all the hook releases every maintenance marker with
`workspace.ClearMarkers` (see Archive), whose failure stops the start.
Before it builds the dependencies, the hook builds the clone workers with
`workspace.NewWorkers(cfg.Workspace.Workers, svc, nil)`, whose refusal stops
the start; after `Bind` it calls their `Start`, so that the clone work left
from before the start is queued before the hub listens (see Clone work),
but runs no clone. The background work `workspace_clones`, which begins
only once the hub listens, launches the workers with `Launch`, so that a
start that fails after the hook (a later start hook, the bind) leaves no
clone running over stores that are closed; it then waits for the stop and
calls the workers' `Stop` with a 10-second deadline, the grace the stop
gives it. The part's one `gitrepo.Git` passes on to git the variables
`GIT_SSL_CAINFO`, `SSL_CERT_FILE`, `HTTPS_PROXY`, `HTTP_PROXY` and
`NO_PROXY` of the hub's environment, read when the hub is composed, and no
other but `PATH` (see [configuration.md](configuration.md#environment-variables)); the
production `Lister` runs its `ListRemote`, and it is `Git`, the
`workspace.Git` (`Clone`, `Open`) of the clone work, and `Fetcher`, the
`workspace.Fetcher` (`FetchOrigin`) of a sync, and `Pusher`, the
`workspace.Pusher` (`Push`) of a push, and `SafetyPusher`, the
`workspace.BranchesPusher` (`PushBranches`) of the safety copy (nil is
`gitrepo.Git{}` for all four; a nil `Scrubber` is a scrubber that knows no value). `Deps` also holds
`InsertWorkspace`, which writes the record in the create transaction (nil
is `workspace.Insert`; a test makes it fail), `BeforeRefSwap`, which a
sync calls between a branch's classification and its swap (nil in
production; a test moves the branch there), `RemoveCopy`, which removes a
workspace's copy once a reclone or a delete has committed (nil is
`gitrepo.Remove`; a test replaces it with a no-op, as a hub that stopped
before the removal, or with one that fails),
and `Notify`, called with the
slug after a create commits so that clone work is picked up; the hub sets
it to the workers' `Enqueue`. `(*Service).Enqueue(slug)` calls `Notify`
(nothing without one): a feature that puts a workspace back to `pending`
calls it once its transaction has committed, since the workers themselves
are the hub's.

`internal/workspace/credential.go` holds `ResolveOriginCredential(ctx,
secrets, slug)`, which every operation that contacts a workspace's origin
calls. It reads the workspace secrets (scope `workspace`, the slug as scope
id) through a `SecretReader` (`Get`, which `*secret.Component` and the
service's `Secrets` have) on every call, one read per name, so that a
changed secret counts at its next use: a non-empty `GIT_PAT` is the user
name `x-token-auth` with the token as password; otherwise `GIT_USERNAME`
and `GIT_PASSWORD`, when both are set; otherwise no credential (the zero
`Credential`). A read error is returned as an error and never taken for no
credential, so that a broken secret store does not turn into an anonymous
request to origin. The names are `SecretGitPAT`, `SecretGitUsername` and
`SecretGitPassword`.

`internal/workspace/create.go` serves `POST /api/v1/workspaces` (scope
`workspaces:write`, a create route, no workspace need; see
[api.md](api.md#post-apiv1workspaces)). The handler refuses the admin token
first, reads the body by an ordered list of members (`createMembers`, each
answering `400 invalid_request` with `<member>: <reason>`, a member able to
depend on those before it; `settings` is applied with
`SettingsRegistry.ApplySettings` from an empty document, and its own
answers pass through unchanged), takes the org `org_id` names
(`identity.OrgBySelector`; unknown is `400`) or else the caller's personal
org (`identity.PersonalOrg`) and checks the caller is an active member of
it (`403` otherwise), answers a slug in use `409 conflict` after a read with
`access.RecordBySlug`. Only a request with a credential then asks the
secret component's `Available()` (false: `secret.UnavailableError`, `503`,
before origin is contacted) and lists origin's refs with the credential
through `Lister` under a 10-second deadline (an error: `400
credential_check_failed` with a fixed message, git's text logged only
through the scrubber as `workspace credential check refused`). Then, in one
store transaction, it inserts the record through `InsertWorkspace` (status
`pending`, a fresh instance id, `created_at` = `updated_at` cut to the
second), stores the credential with the secret component's `Put` as the
workspace secrets `GIT_PAT`, or `GIT_USERNAME` and `GIT_PASSWORD`, and
writes `parley.workspace.create` through the event component; an error of
any of the three rolls all of them back (`500 internal`). A slug taken by a
racing request is `access.ErrDuplicateSlug` from the insert and the same
`409`. The pending record is the clone work; there is no separate queue.

`internal/workspace/response.go` holds `(*Service).RenderWorkspace(ctx, w,
level)`, the one place a workspace object (`workspace.Object`) is built:
every field of the record, a nil one encoded as `null`, plus `org_slug`
(read from the workspace's org), `hub_url` (`ExternalURL` without one
trailing slash, then `/git/<org_slug>/<slug>.git`, or `null` without an
external URL), `access` (the caller's level) and `settings`, the effective
settings of the record, or for a fork those of its parent, read with
`BySlug` at render time so that a change of the parent shows at once (a
fork whose parent has no record shows its own). A record of
kind `fork` goes last through the `ForkView` registered with
`(*Service).RegisterForkView`, so the fork feature completes its object
without this package importing it.

`internal/workspace/read.go` holds `GET /workspaces/:slug`, declared with
`Scope` `workspaces:read` and `Marks` `access.need: read`, so the scope
check and then the access check answer before the handler: the access
check's `404` is the one answer for a workspace the caller cannot read and
for a slug no workspace has. The handler takes the caller's level from
`access.DecisionOf`, reads the whole record with `BySlug` (a record deleted
since the check is the same `404`) and answers it through
`RenderWorkspace`, in every status and for both kinds.

The same file holds `GET /workspaces`, declared with `Scope`
`workspaces:read` and no access need, since it names no workspace; it is on
the binding guard's allowlist. The handler reads the page (`server.ReadPage`),
refuses `include_archived` or `include_forks` with a value other than `true`
or `false` (`400`), and takes the caller's readable set from
`Deps.Access.Readable(ctx, access.CallerFrom(caller))`, which for a
workspace-bound PAT is already cut to its covered set, so the list narrows
with the set and needs no rule of its own. One query (`readList`) then reads
the page's rows: the set's slugs as one JSON parameter (`slug IN (SELECT value
FROM json_each(?))`, every row for the admin token), `status <> 'archived'`
unless asked, `kind = 'repository'` unless forks are asked, or with `parent`
the forks whose `parent_slug` it is when it names a repository workspace
(a parent outside the readable set reads nothing), the cursor's position
`(created_at, slug)` and `ORDER BY created_at DESC, slug ASC LIMIT` the page
plus one. Each row is answered through `RenderWorkspace` with the set's
level for it, and `server.WritePage` writes the page with a cursor of the
last item's stored `created_at` (UnixNano) and slug.

`internal/workspace/update.go` holds `PATCH /workspaces/:slug`, declared
with `Scope` `workspaces:write` and `Marks` `access.need: write`, so the
scope check and then the access check answer before the handler (a member
of the org is `403` in every status, archived included). The handler
decodes the body as an object of raw members, so that a member given as
`null` is told from one not given, then in one store transaction reads the
record with `BySlug` (gone: the same `404`) and judges the body against it
in `applyUpdate`. One table, `updateFields`, says what an update may do with
each field by the workspace's kind: `display_name` and `description` are
mutable on both kinds, `org_id` and `settings` on a repository workspace
only (on a fork `400 not_supported_on_fork`), `ttl`, `purpose` and
`auto_change` on a fork only, through a handler registered for them, and
`slug`, `kind`, `git_url`, `default_branch`, `owner_id`, `workspace_mode`,
`upstream_url`, `integration_branch`, `parent_slug` and `fork_point` on
neither. The checks run in this order, the first failing one answering: an
immutable field, whatever its value; a field that does not apply to the
kind (a fork-only field on a repository workspace, or on a fork with no
handler registered, is `400 invalid_request`); no mutable field; the values
in table order (`display_name`, `description`, `org_id`, `settings`, then
the fork-only fields), each applied to a copy of the record or answering
`400 invalid_request` with `<field>: <reason>` (a value's reader reads
through the update's transaction, and a store error it meets is a `500`,
never a refusal); for an `org_id`, the workspace's owner, not the caller,
an active member of the target org (`identity.ActiveOrgIDs`), else `403
forbidden`; an archived workspace
(`409 workspace_archived`), since validation comes before state; and for a
fork the registered guard. Then it writes `display_name`, `description`,
`org_id`, `settings` and `updated_at` (the hub's time cut to the second),
when the org changed sets `org_id` on every record whose `parent_slug` is
the workspace, runs the fork-only fields' writes and writes the
`parley.workspace.update` event through `Deps.Events` (`updateEventInput`:
the actor from `identity.PrincipalOf`, mapped by `eventActor`, which the
sync event shares: the admin principal to `event.ActorAdminToken` with no
actor id, a user's credential to its type with the user as actor id; `metadata.fields`, the mutable
fields the body carried in `updateFields` order, and for a fork
`metadata.parent_slug`, never a value), all in the same transaction, so a
refusal writes nothing, neither record nor event, and a failed write, the
event's included, keeps nothing. A `null` or empty `display_name` stores the slug, a `null`
`description` the empty string. `org_id` (`moveTarget`) is `null`, the
owner's personal org (`identity.PersonalOrg`), or a string looked up with
`identity.OrgBySelector` (an id in UUID form, else a slug); anything else,
or a string no org has, is `400`. The access rule reads the org from the
same row, so a move changes who may read the workspace and its forks with
no other write. After the commit it reads the record
back and answers it through `RenderWorkspace` with the level
`access.DecisionOf` gives, so the answer is the one a following `GET`
gives.

The fork features add to an update without changing its rules through two
registrations on the service, each under a mutex: `RegisterForkField(name,
ForkFieldHandler)`, for `ttl`, `purpose` or `auto_change` only (another
name panics), whose handler judges the field's raw value and returns the
`ForkFieldWrite` that stores it inside the update's transaction, and
`RegisterForkUpdateGuard(ForkUpdateGuard)`, asked about every update of a
fork after the body is valid and the fork not archived, before anything is
written, its `*server.Error` answered unchanged. `nil` removes either.

`internal/workspace/settings.go` holds `SettingsRegistry`, the setting keys
in registration order, empty in its zero value; the service holds one,
returned by `(*Service).Settings()`. `RegisterSetting(SettingKey)` adds a
key with its `Name`, its `Default` (a function of the default branch), its
`Validate` rule and an optional `Normalize`, the form of an accepted value
that is stored (nil stores the value as given), refusing a duplicate or
incomplete key;
`ApplySettings(stored, raw)` applies a request's settings value (`null`
resets every key; otherwise an object whose unknown keys are refused with
`unknown setting "<key>"`, a `null` value resets that key, every other
value passes its key's rule and is stored normalized; all or nothing, the
first unknown key in byte order named before any refused value, else the
first refused key in registration order); `EffectiveSettings(stored, defaultBranch)`
returns every registered key with its stored value, decoded into the Go
type of the key's default (`bool`, `string`, `[]string`), or its default.
The same file holds the settings table, the one definition of the 21 keys
(name, kind: bool, enum with its strings, URL string or list of branch
globs, and default) in the order they are judged in, and
`RegisterSettingKeys(registry)`, which registers each with its default and
a rule built from its kind: a bool key takes a JSON boolean, an enum key one
of its strings, a URL key a string that is empty or an absolute `https` URL
with a host and no user information, query or fragment once trimmed, stored
trimmed through `Normalize`, and
`protected_branches` an array of at most 50 distinct strings that pass
`ValidateBranchGlob`. Its default is a list holding the default branch, the
empty list (never `null`) while the default branch is `null`.

`internal/workspace/branchglob.go` holds the one branch-glob matcher of the
hub, which every feature reading a list of branch globs calls:
`MatchBranchGlob(glob, branch)` matches the whole name, case-sensitive, `*`
standing for any run of characters except `/`, `**` for any run, every other
character for itself; it runs over the bytes in time proportional to the
product of the two lengths, whatever the stars. `MatchAnyBranchGlob(globs,
branch)` reports whether one glob of a list matches (an empty list matches
nothing), and `ValidateBranchGlob(glob)` accepts 1 to 255 characters that
pass `ValidateBranchName` with each `*` taken as an ordinary character.

`internal/workspace/guard.go` holds `RequireReady(w)`, the state check of
every route that needs a workspace's repository. It reads the marker first:
any marker is `409 workspace_busy`, because a reclone or a delete of a
failed or archived workspace is work the caller should wait out. Then the
status: `pending` or `cloning` is `409 workspace_busy`, `archived` is
`409 workspace_archived` (`server.WorkspaceArchived`), `failed` is
`409 workspace_not_ready`, and `ready` passes. It reads only the record it
is given and takes no lock, so a ready workspace serves any number of
requests at once.

#### Clone work

`internal/workspace/clone.go` holds the clone work. `(*Service).RepoPath(slug)`
is the place of a workspace's copy, `<[workspace].path>/<slug>.git`, and the
one place the path is built. `(*Service).Clone(ctx, slug)` does the clone
work of one workspace, holding the slug's entry of `Service.copies` while
it runs, so that it waits for a delete of an earlier workspace with the
slug to remove that one's copy (see Delete):

1. `StartClone` moves a `pending` workspace to `cloning` and clears its
   `clone_error` before origin is contacted; a workspace already in
   `cloning` is taken up again; any other status, or no workspace, is left
   alone, so a failed clone is never tried again on its own.
2. A copy of this workspace at `RepoPath` is kept: one that opens as a
   repository, whose `parley.workspace` is the workspace's instance id,
   whose `remote.origin.url` is its `git_url`, and whose default branch (the
   copy's own `HEAD` branch while the record has no `default_branch`)
   resolves to a commit. Its head of that branch becomes `head_sha`, the
   branch `default_branch`, the origin head it keeps for it, if any,
   `origin_head_sha`, and the status `ready`, in one write; origin is not
   asked and no post-clone step runs. This is how a hub that ended without
   its stop (killed) between the copy's rename and `ready` is resumed. Anything else at `RepoPath` (a file, an
   empty directory, a repository failing one of the tests, the copy of an
   earlier workspace of the same slug) goes on to a fresh clone.
3. `ResolveOriginCredential` reads the credential; a read error fails the
   clone before git runs.
4. Whatever lies at `RepoPath` is removed, and origin is cloned with
   `Git.Clone` into a directory `.clone-<random>` in the repository root
   (no slug starts with a dot, so no copy carries the prefix).
5. A copy without a branch fails with `origin has no branches`; a
   `default_branch` given at creation that the copy does not have fails with
   `origin has no branch "<name>", the default_branch given at creation`.
   Without one, the default branch is the branch the copy's `HEAD` names
   (origin's default branch), or, when that branch does not exist, the first
   branch by name.
6. `SetOriginHead` writes `refs/remotes/origin/<name>` at the head of every
   branch, the workspace's instance id is written to the copy's
   configuration as `parley.workspace`, the copy's `HEAD` is set to the
   default branch, and the directory is renamed to `RepoPath`.
7. One `UpdateClone` records `default_branch`, `head_sha` and
   `origin_head_sha` (the default branch's head) with the status still
   `cloning`.
8. The post-clone step registered for the workspace's `workspace_mode` with
   `(*Service).RegisterPostCloneStep(mode, step)` runs, given the record and
   the copy opened at `RepoPath`; an error it returns is logged as
   `workspace post-clone step failed` and does not change the outcome. The
   `carry_patch` mode has the set-up of
   [Carry-patch workspaces](#carry-patch-workspaces).
9. The status is set to `ready`, and in the same store transaction every
   clone observer registered with `(*Service).RegisterCloneObserver(o)` is
   told, in registration order, with the transaction and the slug, the copy
   being at `RepoPath`. Each runs under a savepoint: one that fails has its
   own writes undone and is logged as `workspace clone observer failed`,
   and the workspace becomes `ready` all the same. Step 2 tells them too
   when it keeps a copy. The change component is one (see Changes): it
   abandons for `source_lost` the changes whose source branch the fresh
   copy lacks.

Any failure removes the clone directory and whatever lies at `RepoPath`,
logs `workspace clone failed` and sets the status to `failed` with
`clone_error` the failure's text through the hub's scrubber, with the
resolved password as a per-call secret value (secret values replaced by
`[redacted]`, URL userinfo removed, at most 1024 bytes); `head_sha` and
`origin_head_sha` become null and `default_branch` keeps its stored value.
No event is written and no further attempt is scheduled; `Clone` returns
nil once the failure is recorded. When `ctx` ends first (the hub stops),
the clone directory is removed, and so is the copy at `RepoPath` once it
has been renamed there (steps 7 to 9), the status is left as it is so that
the work is taken up again, and `Clone` returns the context's error: a stop
leaves nothing at the copy's place, and the next start clones afresh and
runs the post-clone step. A workspace that is ready is recorded with one
transaction after the step, so a hub that ends without its stop between the
copy's rename and `ready` leaves the workspace in `cloning`, with a copy
step 2 keeps at the next start.

`internal/workspace/worker.go` runs the clone work in the background.
`NewWorkers(size, svc, work)` refuses a size below 1 with an error naming
`[workspace].workers`; `work` is the work run for each slug, `svc.Clone`
when nil. `Enqueue(slug)` appends a slug to a first-in-first-out queue in
memory; a slug already waiting or running is not queued again, and nothing
is queued after `Stop`. `Start(ctx)` removes every `.clone-*` entry of the
repository root (clones a stop interrupted) and every entry
`gitrepo.IsForkTemp` names, a dot-name holding `.tmp-` (the directory of a
fork creation the process did not survive; the stop leaves these alone, as
the hub may still be serving a fork creation then), and queues every workspace in
`pending` or `cloning` in the order of `CloneWork` (by `created_at`, then
slug); it runs no clone. `Launch(ctx)` then starts `size` goroutines that
take slugs from the queue one at a time; the clones run under a context
derived from `ctx`, so they end with it as with `Stop`. Each runs once; a
second call is an error. The queue holds no state of its own: a workspace
in `pending` or `cloning` is its own record of the work, and the next
`Start` reads it again. An error of the work that is not the
end of its context means its outcome could not be recorded: it is logged
as `workspace clone work failed`, and the worker, still holding the slug,
sets the workspace to `failed` with `clone_error` "the outcome of the clone
could not be recorded: <the error>" (scrubbed) and null heads, in one write
that only changes a workspace still in `pending` or `cloning`. When the
store refuses that write too, the worker logs `workspace clone failure not
recorded` and tries again, after 100 ms and then twice as long each time,
up to 30 s, until the store takes it or the workers stop, which leaves the
work to the next `Start`. Once the workspace is failed, its copy at
`RepoPath` is removed. `Stop(ctx)` stops taking work, cancels the
context of the running clones, so that their git processes are killed and
`Clone` leaves their status as it is, waits for the goroutines, then
removes every `.clone-*` entry again and returns the error of that removal
(nil when it succeeds); it returns `ctx`'s error when `ctx` ends before the
goroutines, and nil at once for workers never launched.

#### Sync

`internal/workspace/sync.go` holds the sync of a workspace with its origin.
`(*Service).Sync(ctx, slug, SyncOptions)` reads the record, resolves the
origin credential with `ResolveOriginCredential` at that moment, so that a
changed secret counts at the next sync, and fetches origin into the copy at
`RepoPath` through `Deps.Fetcher` with a deadline of 2 minutes. Two fetches
into one copy both set the origin heads (and `packed-refs`), and must not
interleave there or meet on git's ref locks, so the fetch runs under the
copy's entry of `Service.fetchLocks`, an in-process mutex per
slug (`keyedLocks`, an entry removed when no one holds or waits for it): a
second sync of the workspace waits for the first one's fetch and then runs
its own. The safety copy takes the same entry around its push and the
origin heads it sets, and a reclone around its removal of the copy, so
that a sync that passed the state guard before their marker was set does
not fetch into the copy beside them. The sync route also holds the
workspace's work lock shared for the whole sync (see the maintenance
marker under Archive), which an archive, a reclone and a delete wait for.
Nothing else of a sync takes a lock,
and a sync never writes the
workspace's status: tag creation is create-only (a tag another sync
created first is skipped, not an error) and branch moves are single-ref
compare-and-swaps, so syncs, pushes and landings meet only there. It then
creates in the copy, in name order, every tag origin has and the copy
lacks, at origin's value (an annotated tag as its tag object), each with
`CompareAndSwapRef` expecting the tag absent, so that a tag another writer
creates meanwhile keeps that writer's value; a tag the copy has keeps its
value, whether origin has it at another value, has deleted it or never had
it. It then evaluates every branch origin has, in name order, against the
hub's head of it: `created` when the hub has no such branch, `up_to_date`
when the heads are equal, `fast_forward` when the hub's head is an ancestor
of origin's, `ahead` when origin's head is an ancestor of the hub's, and
otherwise `diverged`, or `reset` when `SyncOptions.ResetToOrigin` is set.
`created`, `fast_forward` and `reset` move the branch to origin's head with
`CompareAndSwapRef` expecting the head that was classified (absent for
`created`); the others move nothing, and a diverged branch does not stop
the walk. A swap that answers `gitrepo.ErrRefMoved` means another writer
moved the branch after it was classified: `(*Service).syncBranch` keeps
that writer's head, reads the branch again, classifies it against the same
origin head and acts on the new result, up to `maxBranchEvaluations` (5)
evaluations; the reported result, `local_sha` and the `Before` of the move
are those of the last evaluation, and only a swap that succeeded is told to
the observers and listed in the event's `updated`. When the fifth swap is
lost too, `Sync` returns `server.Internal` naming the branch, before
`completeSync`: no sync field and no event is written, and the moves made
before stay, their observers already told. `Deps.BeforeRefSwap`, nil in
production, is called between a branch's classification and its swap; it
is for tests, which move the branch there as another writer would. A branch only the hub has, never on origin or deleted there, is
neither looked at nor reported. Protected branches are treated as any
other: sync reads neither the push rules nor `protected_branches` and
`protect.owner_push`. It returns a `SyncResult` holding the record read
again after the completion (see below), `CreatedTags`, the tags it created, and `Branches`, one `BranchResult`
(`name`, `result`, `local_sha`, `origin_sha` as JSON, the hub's head after
the branch was handled and origin's head, full commit ids) per origin
branch it did not skip, sorted by name, empty and never nil when there is
none.

Before the walk, after the fetch and the tags, `excludedBranches` builds
the set of branches the walk skips. For a record whose `Mode` is
`ModeCarryPatch` it holds the record's `IntegrationBranch` (when set) and
the names the registered `ExcludedBranchProvider` returns
(`ExcludedBranches(ctx, slug) ([]string, error)`, asked once per sync);
for any other mode it is empty and the provider is not asked. The provider
is registered with `(*Service).RegisterExcludedBranchProvider`, one at a
time, guarded by its own mutex, nil removing it; this package registers
none, `composeCarryPatch` registers the `*carrypatch.Component`. A skipped branch is
not classified, moved, listed in `Branches` or the event, or told to the
observers; its origin head is still set by the fetch, and `head_sha` and
`origin_head_sha` are read as usual, also when the default branch is the
integration branch. A provider error stops the sync before the walk with
`server.Internal`, its text scrubbed by `Deps.Scrubber`: no branch moved,
no sync field written, no event (tags created by then stay).

A credential that cannot be resolved (nothing is then fetched), a fetch
error and a fetch cut off at its deadline (`gitrepo.ErrDeadline`) are one
failure, handled by `failFetch`: the text `origin fetch failed: ` plus the
error, scrubbed by `Deps.Scrubber` with the credential's password, is
logged (`workspace sync could not fetch`), written as the record's
`sync_error` alone by `recordSyncError` (no other field, no event), and
returned as `server.UpstreamError` with the same text. Nothing in the copy
has changed at that point: a fetch that fails sets no origin head and tags
are created after it.

`internal/workspace/sync_extension.go` declares the extension point of the
carry-patch part of sync: `SyncExtension` with `AfterFetch(ctx, SyncRun)
error` and `BeforeCompletion(ctx, SyncRun, []BranchResult) (SyncAddition,
error)`, registered with `(*Service).RegisterSyncExtension`, one at a time,
guarded by its own mutex, nil removing it; this package imports no
carry-patch package, and the hub registers the `*carrypatch.Component`
(see [Carry-patch workspaces](#carry-patch-workspaces)). `SyncRun` holds the record as read
before the fetch, the caller (`Actor`), `ResetToOrigin` and `OriginHeads`,
a copy of origin's branch heads as fetched. `Sync` takes the extension once,
after the fetch, and only for a record whose `Mode` is `ModeCarryPatch`;
with none, or for a standard workspace, nothing changes. The order is: the
fetch, `AfterFetch`, the tags, `excludedBranches`, the walk,
`BeforeCompletion` (given a copy of `Branches`), `completeSync`. Both
stages run on the sync's context, which the route has detached from the
request. An `AfterFetch` error goes to `failFetch` without the `origin fetch
failed: ` prefix, so a caller tells the two apart: `sync_error` and `502
upstream_error` with the scrubbed text, no branch or tag moved, no event;
the origin heads stay as the fetch, which had succeeded, set them. A
`BeforeCompletion` error is `server.Internal` with its text scrubbed with
the credential's password, as is a member whose value is not valid JSON;
`completeSync` does not run (no field, no event) and the moves made stay.
On success `SyncAddition.UpstreamHeadSHA`, when not nil, is written as
`upstream_head_sha` by `recordSync` in the completion transaction
(`COALESCE`, so nil leaves it), and `SyncAddition.Members` reach the route as
`SyncResult.Members`.

Each `created`, `fast_forward` or `reset` move is reported right after the
ref changed through the exported `(*Service).NotifyBranchMove(ctx,
BranchMove{Workspace, Branch, Before, After, Cause})`, `Before` `""` for a
created branch and `Cause` `MoveCauseSync` (`"sync"`), which a sync
extension calls for its own moves too. It calls every `BranchMoveObserver`
(`BranchMoved(ctx, BranchMove) error`) registered with
`(*Service).RegisterBranchMoveObserver`, once each in the order of
registration (the list is guarded by its own mutex; nil registers nothing);
an observer's error is logged through `Deps.Logger` (`workspace
branch-move observer failed`, with `slug`, `branch`, `cause` and `error`),
and the remaining observers are still called and the sync goes on, since
the branch has moved already. With no observer a sync behaves the same.

When every branch is handled, `completeSync` writes in one `Store.Tx`
`recordSync` (`last_sync_at` = `Deps.Now` in UTC cut to the second,
`sync_error` `NULL`, `head_sha` the copy's head of the record's default
branch and `origin_head_sha` the copy's origin head of it, each `NULL`
when there is none, both read from the copy at that moment rather than
taken from this sync's fetch, so that a sync that completes after an
overlapping later one does not record an older origin than the copy holds; `updated_at` is not written) and the one
`parley.workspace.sync` event (`syncEventInput`), whose actor is
`SyncOptions.Actor` mapped by `eventActor`; a `SyncOptions.Actor` that is
neither the admin token nor a user's credential is an error before
anything is fetched. Its metadata holds the lists `created`,
`fast_forward`, `ahead`, `diverged` and `reset` of branch names (sorted, `[]`
when empty, `up_to_date` branches in none) and `updated`, one `{ref,
before, after}` per moved or created branch and per created tag with full
ref names, `before` `null` for a created ref, sorted by `ref`. `Sync`
returns the record read again after that transaction as
`SyncResult.Workspace`.

`internal/workspace/sync_handler.go` holds `POST /workspaces/:slug/sync`,
declared with `Scope` `workspaces:write` and `Marks` `access.need: write`,
so the scope check and then the access check answer before the handler (a
member of the org is `403` in every status). The handler reads no body. It
reads `reset_to_origin` with `boolParam` (absent or `false`, `true`, else
`400 invalid_request`), reads the record with `BySlug` (gone: the same
`404`), and judges it in `requireSyncable`: a task fork is
`400 fork_has_no_origin`, then `RequireReady`, then the effective setting
`sync_enabled` from the settings registry, `false` being
`400 sync_disabled`, so that nothing is fetched before every check has
passed. It runs `Sync` on `context.WithoutCancel` of the request's context,
so a caller who disconnects does not stop it, with the caller from
`identity.PrincipalOf` as `SyncOptions.Actor`, and answers `200` with the
`RenderWorkspace` object of `SyncResult.Workspace`, the record after the
completion, for the caller's level (`access.DecisionOf`) and the member
`branches`; `syncAnswerJSON` appends each of `SyncResult.Members` after
them, in byte order of the names, leaving out a name the object or
`branches` has already. An error of `Sync` that is a `*server.Error`,
wrapped or not, is the answer; any other is `500 internal`.

`internal/workspace/push.go` holds
`POST /workspaces/:slug/branches/:name/push`, declared like the sync route
with `Scope` `workspaces:write` and `Marks` `access.need: write`, so the
scope check and then the access check answer before the handler looks at
the body. Its handler `servePush` runs `pushBranch`, which sets the origin
credential it resolves, and passes the message of every `*server.Error` it
returns, and the text of any other error, through `Deps.Scrubber.Scrub`
with that credential's `Password` as the per-call secret; the scrubber's
own cut keeps the message within 1024 bytes on a character boundary. `pushForce` reads the body: none or empty is `force` `false`,
otherwise the bytes it read are handed to `server.DecodeJSON` (as the
request's body, so that the decode does not depend on the platform having
kept a copy) into a map, and `force`, when given, must be
`true` or `false` (`400 invalid_request` otherwise, `null` included). The
handler validates the name the router decoded once with
`ValidateBranchName`, reads the record with `BySlug`, and asks
`pushRefusal` in this order: `RequireReady`, a fork (`400
fork_has_no_origin`), for a `carry_patch` workspace its
`integration_branch` (`403 integration_branch`) and then the registered
`PatchBranchLookup` (`403 patch_branch`; an error is `503 unavailable`,
logged scrubbed), and for a forced push `MatchAnyBranchGlob` over the
effective `protected_branches` of the settings registry (`403
protected_branch`); none of them needs the branch or contacts origin. The
lookup is registered with `(*Service).RegisterPatchBranchLookup`, one at a
time, by the feature that keeps a carry-patch workspace's patch list
(`composeCarryPatch` registers the carry-patch component);
without one no branch is a patch branch. The handler then reads the
branch's head once with `ResolveBranch` (no branch is `404
branch_not_found`), resolves the credential with
`ResolveOriginCredential` and hands one `gitrepo.PushRequest` to
`Deps.Pusher`; it takes no lock. For a forced push the request's `Lease`
is the branch's recorded origin head (`OriginHead`), or `""`, origin must
not have the branch, when none is recorded; `gitrepo.Git.Push` passes it as
`--force-with-lease=refs/heads/<name>:<lease>` and never a plain force, and
reads git's `+` status as `forced` and its stale-info refusal as an error
matching `gitrepo.ErrOriginMoved`, which the handler answers `409
origin_moved` without touching the recorded origin head. The push runs
under a context with `Deps.PushDeadline` (`Bind` makes 0 the fixed 2
minutes, `pushDeadline`); a push whose error wraps
`context.DeadlineExceeded`, which `gitrepo` returns when it killed git's
process group at the deadline and waited for it, is `504 origin_timeout`;
a git that ended on its own as the deadline passed is answered by what it
reported. `pushFailure` maps a `*gitrepo.PushError`
by its class: `ErrOriginRejected` to `422 origin_rejected` with origin's
reason, `ErrUnauthenticated` to `502 origin_unauthenticated` (the message
says `no credential` when the resolved credential is zero, otherwise that
origin `rejected` the stored one), `ErrForbidden`, `ErrRepositoryNotFound`
and `ErrUnreachable` to `502 origin_forbidden`, `origin_not_found` and
`origin_unreachable`, and anything else to `502 upstream_error`. A
credential that cannot be read is `500 internal` before the push
operation is called. It answers `200` with `branch`, `sha`,
`remote` `origin`, `remote_url` (`withoutUserinfo` of `git_url`: an https
URL without user information, any other form as stored, then scrubbed with
the push's credential, computed once and used by the answer and the event
alike) and `result`; an
error matching `gitrepo.ErrNonFastForward` is `409 non_fast_forward`,
whose message names the origin head the copy keeps for the branch
(`OriginHead`), read without asking origin. The handler reads that origin
head once, after the credential and the caller (`eventActor`), and from
there on works on `context.WithoutCancel` of the request's context, so a
caller who disconnects ends neither the push, which only
`Deps.PushDeadline` bounds, nor its records. Every push handed to origin
writes one `parley.workspace.push` event (`pushOp.event`, the one place
its texts are put together: `branch`, `sha`, `force`, `ok`, `remote_url`
and `result` or `error_type`, the branch passed through `pushOp.scrub`,
the scrubber with the push's credential, and `remote_url` the scrubbed one
of the answer) with `writePushRecords`, one `Store.Tx`; when that
transaction fails, the answer is `500 internal` and no event is written. A
failure writes the event alone. A push origin took goes through
`recordPushed`: `SetOriginHead` of the branch to the pushed commit first,
since a ref cannot be part of a store transaction, then the transaction
with the event and, for the default branch, `recordOriginHead`
(`origin_head_sha` alone, never `updated_at` or `status`); when the
transaction fails the ref is put back in one operation and the answer is
`500 internal`: when a head was recorded before the push,
`CompareAndSwapRef` sets it back to that head only while the ref still
holds the pushed commit (`ErrRefMoved` means a sync moved it since, and
the sync's value is kept); when none was, `DeleteOriginHead`. Any other
error of the restore is logged.

#### Safety copy

`internal/workspace/safetycopy.go` holds the safety copy an archive or a
reclone makes once its checks have passed, `(*Service).safetyCopy(ctx,
w)`, which returns the `origin_push` of the operation's event and never an
error: `ok`, `partial` or `failed`. A workspace in status `failed`, one
without a `git_url` or one with nothing at `RepoPath` is `ok` without
calling git. Otherwise it opens the copy with `Deps.Git`, takes every
branch with its head from `Branches`, drops the `integration_branch` of a
`carry_patch` workspace, and with no branch left is `ok`; it resolves the
credential with `ResolveOriginCredential` (an error is `failed` before
any git call, never an anonymous push) and hands every branch to
`Deps.SafetyPusher` in one `gitrepo.PushBranchesRequest` under a context
with `Deps.PushDeadline`, the push route's deadline, holding the copy's
entry of `Service.fetchLocks` (see Sync) until it returns. Each branch origin
took (`pushed` or `up_to_date`) has its origin head set to the commit
offered with `SetOriginHead`; a refused one keeps its own, and no
`parley.workspace.push` event and no `origin_head_sha` is written. The
outcome is `ok` when origin took every branch offered, `partial` when it
took some, and `failed` when it took none, including a transport failure
or a deadline with no branch taken. A failure (the copy that cannot be
read, the credential, the refused branches and the push's error) is
logged as `workspace safety copy failed` with the slug and the step, its
text passed through the scrubber with the credential. A refused branch and
the push's error are logged only by their class (`pushReason`): the
`Class` of a `*gitrepo.PushError`, `ErrNonFastForward`'s text, `the push
did not finish within its deadline`, or `ErrPushFailed`'s text for any
other error, never what git or origin wrote, which may hold anything
origin chose to send.

#### Archive

`internal/workspace/archive.go` holds `POST /workspaces/:slug/archive`
(scope `workspaces:write`, `access.MarkNeed` `write`, so that only the
owner and the admin token reach the handler; the body is not read). The
handler reads the record and refuses, in this order, a fork (`400
not_supported_on_fork`), then, through `RequireReady`, a set marker or a
status `pending` or `cloning` (`409 workspace_busy`) and `archived` (`409
workspace_archived`); `ready` and `failed` pass. It then detaches from the
request's cancellation (`context.WithoutCancel`) and runs the archive with
the marker `archive` held. Under the marker it reads the record again and
checks the status again, makes the safety copy, and in one store
transaction sets `status` `archived`, clears `clone_error`, sets
`updated_at`, counts the rows whose `parent_slug` is the workspace, and
emits `parley.workspace.archive` (actor as for the other workspace
events, `resource_type` `workspace`, `metadata` `{head_sha, origin_push,
forks}`, `head_sha` the record's, `null` when it has none). A failure of
the transaction is a `500 internal` with nothing changed. The answer is
the record read after the commit, rendered by `RenderWorkspace`. The copy
is read only by the safety copy, and no fork record or other data is
touched: no remover of owned data and no sandbox remover is called.

The archive takes its collaborators from the teardown set once, before the
safety copy. In its transaction, after the status write and before the
event, it lists the forks (`SELECT slug FROM workspaces WHERE parent_slug =
? ORDER BY slug`), whose number is the event's `forks`; after the event,
when a session closer is registered, it calls
`CloseSessions(ctx, tx, [slug, forks...], "workspace archived")` under the
savepoint `archive_sessions`: a closer's error is logged as `workspace
sessions not closed on archive` with the slug, `ROLLBACK TO
archive_sessions` undoes the closer's writes, and the archive commits; only
a failure of the savepoint statements fails it. After the commit every
archive observer is called in registration order with the slug and the
forks; an observer's error, or its panic, recovered as an error carrying
the panic value (`guarded`), is logged as `workspace archive observer
failed` with the slug and the next observer is called: the archive
stands, and the answer is `200`. The answer is the record read again
(`reread`); a read that fails is logged as `workspace record not read
again after the archive` with the slug, and the answer is the record read
under the marker with the committed `status`, `clone_error` and
`updated_at`. A refused archive calls neither.

`internal/workspace/teardown.go` holds the teardown set
([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)),
`workspace.Teardown`, which `(*Service).Teardown()` returns: the
collaborators through which an archive and a delete reach the data other
packages keep for a workspace. Its zero value is valid, and with nothing
registered the operations touch the workspace records alone. It holds one
`TokenRevoker` (`RevokeBoundTo(ctx, tx, slugs)` returning the revoked
token ids; `RegisterTokenRevoker`), the `Remover`s of owned data
(`func(ctx, tx, slug) error`, in registration order; `RegisterRemover`),
one `SessionCloser` (`CloseSessions(ctx, tx, slugs, reason)`;
`RegisterSessionCloser`), one `SandboxRemover` (`RemoveSandboxes(ctx,
slugs)`; `RegisterSandboxRemover`), the `ArchiveObserver`s
(`WorkspaceArchived(ctx, slug, forks)`, in registration order;
`RegisterArchiveObserver`) and one `ForkRemover` (`DeleteFork(ctx, slug,
caller)` and `RemoveForksOf(ctx, tx, parent, actor)` returning the removed
slugs and a function that discards their repository data after the
commit; `RegisterForkRemover`). A single collaborator's `Register` replaces
the one before, and nil removes it; a list's adds, and nil adds none.
Registration is safe from several goroutines and happens while the hub is
composed. `(*Service).RemoveOwned(ctx, tx, slug, reason)` removes in `tx`
what the workspace owns outside its record: it calls the token revoker
with `[slug]`, every remover of owned data in order and the session closer
with `[slug]` and the reason, stops at the first error, and returns the
revoked token ids in ascending order, `[]` when there are none. The
workspaces part registers on the set `c.access` (the `*access.Service`) as
the token revoker and `secret.RemoveWorkspace` and
`secret.RemoveWorkspaceVars` as removers of owned data.

`internal/workspace/maintenance.go` holds the maintenance marker, the
record's `maintenance` column, which `RequireReady` reads (see errata 14:
the marker is stored, not kept in memory). `(*Service).withMarker(ctx,
slug, m, fn)` sets it with one conditional write (`UPDATE workspaces SET
maintenance = ? WHERE slug = ? AND maintenance = ''`), so that of two
operations that ask at once exactly one holds it; the other is answered
by `RequireReady` on the record read again (`409 workspace_busy`), or
`404` when the workspace is gone, without `fn` running. It releases the
marker in a deferred write on a context that is not cancelled, only while
it is still `m`, when `fn` returns or panics; a failure to release is
logged. Neither write changes `updated_at`. Once the marker is set, `fn`
runs holding the workspace's entry of `Service.work` (`keyedLocks`, a
readers-writer mutex per slug) alone, so it first waits for the syncs and
pushes of the workspace still running. Those hold the same entry shared
for their whole run: `(*Service).admitWork(ctx, slug, check)`, which the
sync and push handlers call in place of their own read, reads the record
and judges it with the route's refusals (state guard included), takes the
entry shared, then reads and judges the record again under it. The first
read answers a request that comes while an operation runs at once
(`409 workspace_busy`). The second read refuses one that read the record
just before the marker was set and then waited for the operation to end,
since the operation has changed the record. `ClearMarkers(ctx, ex)`
releases every marker; the start hook `workspaces` calls it, so that a
hub that stopped during an operation starts with no marker.

#### Reactivate

`internal/workspace/reactivate.go` holds `POST /workspaces/:slug/reactivate`
(scope `workspaces:write`, `access.MarkNeed` `write`, as for the archive;
the body is not read). The handler reads the record and refuses
(`reactivateRefusal`), in this order, a fork (`400 not_supported_on_fork`),
a set marker (`409 workspace_busy`, `RequireReady`'s message), and every
status other than `archived`, `pending` and `cloning` included (`409
workspace_not_archived`). It holds no marker: it only writes the record.
Detached from the request's cancellation, in one store transaction it sets
`status` `pending`, clears `clone_error` and sets `updated_at` with the
conditional write `UPDATE workspaces SET ... WHERE slug = ? AND status =
'archived' AND maintenance = ''`, so that it cannot slip in beside an
operation that holds the marker, and emits `parley.workspace.reactivate`
(actor as for the other workspace events, `resource_type` `workspace`,
`metadata` `{}`). A write that changes no row is answered from the record
read again (`lostReactivate`: the refusal it now gets, `404` when it is
gone, else `409 workspace_busy`); any other failure of the transaction is
a `500 internal` with nothing changed. After the commit it reads the
record again, then calls `(*Service).Enqueue(slug)`, so that the answer,
rendered by `RenderWorkspace`, is the record as the reactivate left it,
`pending`, whatever the clone work has done since. The clone work then keeps the copy the archive left when
it is valid for the record (`keepCopy`: it opens, its instance id and
origin url are the record's and the default branch resolves), so that the
workspace is `ready` again without a request to origin, and makes a fresh
clone otherwise.

#### Reclone

`internal/workspace/reclone.go` holds `POST /workspaces/:slug/reclone`
(scope `workspaces:write`, `access.MarkNeed` `write`, as for the archive;
the body is not read). The handler reads the record and refuses
(`recloneRefusal`), in this order, a fork (`400 fork_has_no_origin`), then
through `RequireReady` a set marker or `pending`/`cloning` (`409
workspace_busy`) and `archived` (`409 workspace_archived`), then a
workspace with at least one fork row (`parent_slug` = slug; `409
has_forks`). Then it moves off the request's cancellation and, holding the
`reclone` marker (`withMarker`), reads the record again, checks it again,
makes the safety copy, and in one store transaction sets `status`
`pending`, clears `sync_error` and `origin_head_sha`, gives the record a
new `instance_id` (`NewInstanceID`), sets `updated_at` and emits
`parley.workspace.reclone` (actor as for the other workspace events,
`resource_type` `workspace`, `metadata` `{origin_push}`). `head_sha`,
`default_branch`, `last_sync_at` and `clone_error` stay until the clone
work changes them. A failed transaction answers `500 internal` and changes
neither the record nor the copy. After the commit it reads the record for
the answer (`reread`: a read that fails is logged as `workspace record not
read again after the reclone` with the slug, and the answer carries the
committed values instead), removes the copy through `Deps.RemoveCopy(RepoPath(slug))`
under the copy's entry of `Service.fetchLocks`, which removes the origin heads (`refs/remotes/origin/*`) with it (a
failure or a panic, recovered by `guarded`, is logged as `workspace copy
not removed after a reclone` with the slug and the path), and calls
`(*Service).Enqueue(slug)` whatever the removal did; the answer is
rendered by `RenderWorkspace`.
The new instance id is what makes a reclone safe against a stop between
the commit and the removal: a copy that survives is not valid for the
record (`keepCopy`), so the clone work at the next start replaces it with
a fresh clone. No remover of the teardown set is called: change records
and every other package's data stay.

#### Delete

`internal/workspace/delete.go` holds `DELETE /workspaces/:slug` (scope
`workspaces:write`, `access.MarkNeed` `write`, as for the archive; the body
is not read). The handler reads the record. A task fork goes to
`deleteFork`: it hands the slug and the caller to `DeleteFork` of the
teardown set's fork remover, the one place forks are removed, on a context
the request's end does not cancel, and answers `204` with no body when it
returns nil and its error otherwise (a `*server.Error` unchanged, any other
error `500 internal`); it sets no marker and runs none of the removal below.
With no fork remover registered a fork is answered `500 internal` and
nothing is removed. A repository workspace is refused (`deleteRefusal`), in
this order, with a set marker (`409 workspace_busy`, `RequireReady`'s
message) and with every status other than `archived` (`409
workspace_not_archived`). Then it moves off the request's cancellation and,
holding the `delete` marker (`withMarker`), reads the record again, checks
it again and takes the teardown set once, whose collaborators every
removal of the operation then calls. It lists the forks (`forksOf`,
ascending) and, when a sandbox remover is registered, calls
`RemoveSandboxes(ctx, [slug, forks...])` before any write; its error is a
`500 internal` with nothing changed. It then takes the entries of
`Service.copies` (`keyedLocks`) of the workspace and those forks, in slug
order, and holds them until the copies are removed and the discard
function has run. Clone work holds its slug's entry for its whole run, so
a workspace created again with the slug once the transaction has
committed has its clone wait for the removal of the old copy, and the
removal never meets the new copy. Then, in one store transaction: with a
fork remover, `RemoveForksOf(ctx, tx, slug, caller)` removes the forks and
writes their events, and its slugs, sorted, are the `deleted_forks`;
without one, each fork in ascending slug order is removed by
`removeWorkspace`, the same per-workspace removal as the workspace's. That
removes what the workspace owns as `RemoveOwned(ctx, tx, slug, "workspace
deleted")` does, with the delete's set rather than a snapshot of its own
(`removeOwned`, which `RemoveOwned` wraps), deletes the record
(`DELETE FROM workspaces WHERE slug = ?`, its settings with it) and emits
`parley.workspace.delete` (actor as for the other workspace events,
`resource_type` `workspace`, `metadata` `{kind, parent_slug,
revoked_tokens}`, `revoked_tokens` being what `RemoveOwned` returned). A
fork's event passes `event.Input.Parent`, because the fork's record is
already gone when it is written. Last, the workspace itself goes the same
way, its event with `deleted_forks` as well. A removed fork that was not in
the list read before the sandboxes went, a fork made while the delete ran
whose sandbox may still run, rolls the transaction back with `409
workspace_busy`, and the delete can be repeated. Any other error rolls the
whole transaction back and is a `500 internal`, and no repository data is
discarded. After the commit `Deps.RemoveCopy(RepoPath(ws))` removes the
copy of each fork the transaction removed itself and then the workspace's
(a failure is logged as `workspace copy not removed after a delete` with
the slug and the path, and the answer stays `204`), and then the fork
remover's discard function runs. The answer is `204` with no body. No
event of the log is removed, so the earlier events of the deleted
workspaces stay readable, and the slug can be created again.

#### Task forks

`internal/fork/store.go` holds `fork.SchemaStep` (`fork_fields`), which adds
to the table `workspaces`, after `workspace.SchemaStep`, the columns
`tracking_branches` and `tracking_tags`: the names of the branches and of
the tags the parent had when the fork was created, each a JSON array of
names, sorted and without repeats, NULL for a repository. They are columns
of the workspace record, not a table of their own, so they go with the
fork's record when it is removed. A fork's base branch is its
`default_branch`; its fork point, `expires_at`, `auto_change`, `purpose`,
`session_id`, parent and creating token are columns of the workspace
record already. The record type is `fork.Fork` (an embedded
`workspace.Workspace` plus `TrackingBranches` and `TrackingTags`), and the
store functions are:

- `Insert(ctx, tx, f)` stores the fork through `workspace.Insert` in the
  caller's transaction and then its tracking names, so a slug in use is
  `access.ErrDuplicateSlug` and a record the access rule refuses
  `access.ErrInvalidRecord`; a record not of kind `fork` is refused with
  `access.ErrInvalidRecord` before anything is written.
- `BySlug(ctx, q, slug)` reads a fork through an `access.Querier`; a slug
  no workspace has and a repository workspace are found = false with a nil
  error.
- `CountOf(ctx, q, parent)` counts the forks of a parent, and
  `ListOf(ctx, q, parent, f)` lists those the `fork.ListFilter` `f` selects,
  newest first, those created at the same instant by slug: only the slugs
  of `Slugs` when it is not nil, the forks of `OwnerID`, those created in
  `SessionID` (compared in lower case), those after the `fork.Position`
  `After` (`created_at` in UnixNano, then slug), at most `Limit` of them;
  the zero filter lists them all. The selection, the order and the position
  are one query, so a page reads only its own rows.

`internal/fork/refs.go` holds what the `fork.Component` answers about a
fork's refs, each read now, from the parent's repository and the fork's,
at `(*workspace.Service).RepoPath`; nothing is copied into the fork for it:

- `Refs(ctx, slug)` is the ref view (`RefView`, with `Branches` and
  `Tags`, each ordered by name and never nil, and the lookups `Branch(name)`
  and `Tag(name)`), the one answer to which refs a fork has. Each recorded
  branch name is a tracking branch (`Branch{Name, SHA, Tracking: true}`) at
  the parent's current head of that name, absent while the parent has no
  branch of the name; each recorded tag is listed (`Tag{Name, SHA}`) at the
  object the parent's tag of the name points at now (the tag object of an
  annotated tag), absent while the parent has none; a branch or tag the
  parent gained after the fork never shows. The branches of the fork's own
  repository are its own branches (`Tracking: false`), except those under
  a recorded tracking name: git serving a fork copies the parent's tracking
  refs into its repository, and such a copy is never an own branch, so a
  recorded name is listed once, as the tracking branch at the parent's
  head. Refs outside `refs/heads/`, such as `refs/parley/changes/*`, are
  never branches. A slug that names no fork is an error matching
  `fork.ErrNotFork`.
- `HeadSHA(ctx, slug)` is the parent's current head of the fork's base
  branch, nil when the parent has no such branch; the recorded fork point
  is never read for it.
- `CheckBranchWrite(ctx, slug, branch)` refuses the creation, move or
  deletion of a branch (named without `refs/heads/`) of a fork whose name is
  one of its recorded tracking names with `fork.TrackingBranchError`, `403
  tracking_branch`, also while the parent has no branch of that name, since
  the name stays reserved; it returns nil for every other name and for every
  branch of a workspace that is not a fork.

`internal/hub/fork.go` adds the part `composeForks`, after
`composeWorkspaces`. It creates the `*fork.Component`, registers
`fork.SchemaStep` and registers the component's `WorkspaceView` as the
workspace service's fork view (`RegisterForkView`), so that every workspace
object of a fork, as `GET /workspaces/:slug` answers it, has the `head_sha`
`HeadSHA` gives, registers the update handlers of `purpose` and
`auto_change` and the update guard (`RegisterForkField`,
`RegisterForkUpdateGuard`), and adds the component's routes (`Routes`); its start
hook `forks` builds the component's `fork.Deps` (the stores, the workspace
service, the hub's git with the variables of the hub's environment that
every git process gets, the event component as `Events`, the access rule as
`Access`, whose `MintBound` mints a fork's token, and as `Readable`, whose
readable set confines the fork list, `rt.Config.Forks` as
`Limits`, the hub's clock, `CreateRepo` = the git's `CreateFork` and
`Transact` = `rt.Store.Tx`, `rt.Logger` as `Logger`), applies every
function `AdjustForks` added, and binds the component; `Bind` sets
`NewName`, when nil, to the generator of 8 characters from `a` to `z` and
digits drawn from `crypto/rand`, `RemoveRepo`, when nil, to
`gitrepo.Remove`, `Logger`, when nil, to a logger that discards, and
`SweepTicks`, when nil, to a `time.Ticker` of `fork.SweepInterval`
(60 seconds). The
part also registers `fork.DiscardStep` after `fork.SchemaStep` and, at
compose time, the component as the fork remover of the workspace service's
teardown set (`Teardown().RegisterForkRemover`), so that `DELETE
/workspaces/:slug` of a fork and the delete of a parent reach it. No
sandbox lifecycle is registered while the hub has no sandbox feature. The
part registers `fork.ActivityStep` after `fork.DiscardStep`. The change
part registers the change reader (`RegisterChangeReader`, see
[Changes](#changes)). Of the features that see a fork's activity (the push
rules, the change feature, the landing), the push rules and the change
feature are in the hub: the part registers the component as the ref
writer's fork activity recorder (`c.RefWriter().RegisterForkActivity`),
which the writer calls after an accepted write through git or the source
control API to a fork, outside any store transaction, and the change part
registers `(*fork.Component).RecordActivityIn` as the change component's,
which runs in the transaction that opens or abandons a change from the
fork (`RecordActivity` opens a transaction of its own, which inside
another would wait for it forever). The landing part registers an
after-landing hook that calls `RecordActivity` for each landed change
whose source is not the target (see [Landing](#landing)). It
adds the background work `fork_sweep` (grace 10 seconds), which runs
`(*Component).RunSweep` from the bind to the stop, and the maintenance task
`fork_expiry`, which runs one `RunPass` in each maintenance pass.

`internal/fork/remove.go` holds the removal of a fork, the one place forks
are removed, whatever the cause: `deleted` (a `DELETE`), `expired` and
`parent_deleted`. The step inside the transaction (`removeIn`) reads the
fork through the transaction (none is the not-found result), removes what
it owns with `(*workspace.Service).RemoveOwned(ctx, tx, slug, reason)`
(the token revoker, every remover of owned data, `secret.RemoveWorkspace`
and `secret.RemoveWorkspaceVars` among them, and the session closer, with the reason `fork expired` for
the cause `expired` and `workspace deleted` otherwise), deletes the record
(`DELETE FROM workspaces WHERE slug = ? AND kind = 'fork'`; no row deleted
is the not-found result, so that of two removals of one fork the loser
writes nothing), and emits the event through `Events` with `Parent` set:
`parley.workspace.delete` (`fork.EventDelete`) by the caller, metadata
`kind` `fork`, `parent_slug` and `revoked_tokens` in ascending order, for
`deleted` and `parent_deleted`, and `parley.fork.expire`
(`fork.EventExpire`) by the system for `expired`; both carry the fork's
`session_id`. A removal of one fork (`remove`) takes the parent's create
lock first and holds it to its end, then refuses a parent that is
`pending` or `cloning` or carries a maintenance marker with `409
workspace_busy` (any other parent status lets the fork go), then, when a
`SandboxLifecycle` is registered, calls `DeleteSandboxes(ctx, slug,
"workspace_deleted", false)` before any write and stops on its error;
then runs `removeIn` in one `Transact`, which checks the parent again
through the transaction, and once it has committed discards the fork's
repository at its `RepoPath` with `RemoveRepo`, which removes its own
branches with it. A `ttl` update or a parent's archive takes no create
lock, so a fork whose transaction backs out (no longer due, or its parent
busy) after its sandboxes were deleted is kept without them, and that is
logged at warn level (`fork kept after its sandboxes were deleted: it
changed while they were`, with `slug` and `error`). A discard that
fails is logged and recorded in the table `fork_discards` (`slug` primary
key, `parent_slug`, `recorded_at` in UnixNano), created by
`fork.DiscardStep`; the removal's result does not change. `DeleteFork(ctx,
slug, caller)` is that removal for a `DELETE`: nil on success, `404
not_found` (`workspace not found`) for the not-found result, the `409` of
its own refusal of a busy parent unchanged (that refusal carries the
sentinel `errParentBusy`), and `500 internal` (`internal error`, the cause
logged) for anything else, a `409` of a remover of owned data or of the
session closer included. `RemoveForksOf(ctx, tx, parent, actor)` runs `removeIn`
with the cause `parent_deleted` for every fork of the parent in ascending
slug order inside the parent's transaction, without the sandbox call or a
transaction of its own, and returns the slugs (`[]` for none) and the
function that discards their repositories. `CountTaskForks(ctx)` counts
the records of kind `fork`.

`internal/fork/sweep.go` holds the sweep. `(*Component).RunPass(ctx)` is
one pass: it reads, at `Deps.Now`, the slugs of the records of kind `fork`
whose `expires_at` is at or before that time, in ascending order, and
removes each through `remove` with the cause `expired` and the reason
`ttl`; the fork is read again before its sandboxes are deleted and inside
the transaction, and one whose `expires_at` is no longer due (given a new
lifetime since the pass read it) is left alone. A not-found result and the
removal's refusal of a busy parent leave the fork for a later pass without
an error log; any other failure, a collaborator's `409` included, is logged
at error level (`fork sweep: fork not removed; tried again at the next
pass`, with `slug` and `error`), the fork is whole, and the pass goes on.
`ctx` is checked before each fork and before each part of the pass, and
once it is cancelled the pass ends, with `Stopped` set and no error log for
a read the cancellation broke; the removal itself runs on
`context.WithoutCancel(ctx)`, so that a removal under way commits or rolls
back and discards. At the end of a pass that was not stopped, each row of
`fork_discards` is retried under the create lock of its parent: while no
workspace record has the slug the repository at its `RepoPath` is removed
with `RemoveRepo` and the row is deleted once that succeeds (a failure is
logged and the row kept); once a record has the slug again the row is
deleted without a removal, since that fork's create replaced the leftover
directory with its own repository. A pass that finds another running
returns at once with `PassResult.Skipped`; `PassResult` also lists the
slugs `Removed` and `Failed` and says whether the pass was `Stopped`.

`internal/fork/early.go` holds the early expiry, the part of a pass that
runs after the expired forks and before the pending discards, and only
while `Deps.Limits.ExpireOnLand` (`[forks].expire_on_land`) is set; while
it is not, the change reader is never asked. It reads the slugs of the
records of kind `fork` whose `expires_at` is after the pass's time, in
ascending order, so that a fork past it goes once, with the reason `ttl`,
and judges each: its last activity (`last_activity_at`, UnixNano, a column
of the workspace table added by `fork.ActivityStep`, `fork_activity`;
NULL stands for the fork's `created_at`) is at least 24 hours before the
pass's time; the registered `ChangeReader`'s `ChangesFrom(ctx, slug)`
returns at least one change and each is `landed` or `abandoned`; each of
the fork's own branches, read from its repository, is the `Branch` of one
of them; and the `SandboxLifecycle`'s `HasLiveSandbox(ctx, slug)` is
false. Without a change reader a fork has no changes, and without a
sandbox lifecycle no sandboxes. A fork that passes is removed through
`remove` with the cause `expired` and the reason `landed`, with the same
handling of a busy parent and of a failure as an expired fork; its due
rule reads the fork again before the sandboxes and inside the
transaction and leaves it alone once its `expires_at` has passed or an
activity has been recorded since it was judged, since every event that
changes the other conditions is an activity. An error of the change
reader or the sandbox lifecycle is logged at error level (`fork sweep:
early expiry of the fork not judged; the fork is kept`, with `slug` and
`error`) and the pass goes on with the next fork.
`(*Component).RecordActivity(ctx, slug)` sets `last_activity_at` to
`Deps.Now` for the record of kind `fork` with the slug, in a `Store.Tx` of
its own, and changes nothing for any other slug.

`(*Component).RunSweep(ctx)` starts the ticker of `Deps.SweepTicks`, runs
one pass at once and one at each tick, and returns once `ctx` is cancelled
and the running pass has ended.

`internal/fork/create.go` holds the create, `POST /workspaces/:slug/forks`
(see [the API](api.md#post-apiv1workspacesslugforks)). The route declares
the scope `workspaces:write`, the access need `read` and `ShowOnce`
`token.token`, so that a replay of its `Idempotency-Key` answers without the
token's secret. The handler refuses the admin token before it reads the
body, reads the body member by member (no body is `{}`, a `null` member is
absent), computes `expires_at` with `ResolveTTL` or `DefaultExpiry` at the
hub's time, reads the parent's record and judges it with
`workspace.RequireReady`, reads the parent's branches and tags from its
repository and takes the fork point from them (`400 fork_ref_not_found`
for a base branch the parent does not have). From the name check to the
commit it holds a lock per parent slug (`Component.createLocks`), so that
the creates of one parent's forks run one at a time there: it picks the
slug (`<parent>--<name>`, `409 conflict` when a workspace has it, or a
generated name drawn again while it is in use, at most 8 draws, then `500
internal`), removes whatever is at the fork's `RepoPath` (no record has the
slug, so it is a leftover of a create that stopped), makes the repository
with `CreateRepo`, renders the workspace object with access `owner`, and
in one `Transact` reads the parent again and judges it again with
`RequireReady`, so that a maintenance operation that began meanwhile
refuses the create, inserts the fork with `fork.Insert` (status `ready`,
the caller's user as owner, the parent's org, the creating PAT's id when a
PAT authenticated the request), records the link of the fork to its
session, mints the token through `Access.MintBound` when the body has one,
writes `parley.workspace.fork` (`fork.EventFork`) through `Events`, and
last asks the sandbox component when the body has a `sandbox`. When the
repository creation or the
transaction fails, the repository directory is removed with
`gitrepo.Remove` and the answer is the error, `500 internal` for anything
but an API error; a duplicate slug at the insert is `409 conflict`.

`internal/fork/list.go` holds the fork list, `GET /workspaces/:slug/forks`
(see [the API](api.md#get-apiv1workspacesslugforks)). The route declares
the scope `workspaces:read` and the access need `read`, and no state
precondition. The handler reads the page with `server.ReadPage`, refuses an
`owner` other than `me` and a `session_id` that is not a UUID (each given
once) with `400 invalid_request`, and builds the caller's readable set with
`Readable(ctx, access.CallerFrom(caller))`, which for a PAT bound to a
workspace is already its binding's covered set: `ListOf` reads the forks
of the slug among that set's slugs (every fork for the admin token's set),
filtered and after the cursor's `fork.Position`, one more than the page
holds, and each is rendered with `(*workspace.Service).RenderWorkspace` at
the set's level on it, so its `head_sha` comes through the fork view;
`server.WritePage` writes the page with the last item's position as
`next_cursor`. `owner=me` with the admin token, which acts for no user,
answers an empty page without a read.

`internal/fork/collaborators.go` declares the two collaborators of the
create, which other features supply and register on the component while
the hub is composed (`RegisterSessionLinker`, `RegisterSandbox`; nil
removes one), so that `internal/fork` depends on neither:

- `SessionLinker` has `Check(ctx, caller, sessionID, parent)`, which
  returns the id in lower case when it accepts it, an error matching
  `fork.ErrUnknownSession` when it refuses it and any other error when it
  cannot tell, and `Record(ctx, tx, sessionID, parent, forkSlug)`, which
  links the fork to the session in the fork's creating transaction. The
  create asks `Check` after the body, before the parent is read (`400
  unknown_session` for a refusal, `500 internal` for a failure), stores the
  id it returns on the fork, so that the fork's event carries it, and calls
  `Record` right after the insert, its error rolling the transaction back
  (`500 internal`). Neither is called without a `session_id`; without a
  linker every `session_id` is `400 unknown_session`.
- `SandboxCreator` has `CreateSandbox(ctx, tx, fork.SandboxRequest)`, the
  request holding the caller, the fork, the body's `sandbox` member as
  received and the fork's `session_id` and `purpose` as defaults; it
  returns the sandbox object, a JSON object, or the API error the create
  answers unchanged (status, `error_type`, extra members). The create calls
  it once, the last step of its transaction, after the fork, its session
  link, its token and its event are written and its repository exists, and
  never reads the member itself; an error rolls the transaction back and
  removes the repository directory, a result that is not a JSON object is
  `500 internal`, and the object is the answer's `sandbox`. Without a
  component a `sandbox` is `404 sandboxes_disabled` at that place.

`internal/fork/guard.go` holds `CheckParentState(ctx, slug, use)` of the
component, `use` being `fork.UseRead` or `fork.UseWrite`, the check every
route that reads or writes a fork makes of the fork's parent, its errors
built with the constructors of `internal/server` so that a caller answers
them unchanged: nil for a repository workspace and for a fork whose parent
is `ready` with no maintenance marker; otherwise `workspace.RequireReady`
of the parent (`409 workspace_busy` for a marker, `pending` or `cloning`,
`409 workspace_not_ready` for `failed`), except that an `archived` parent
with no marker allows a read and refuses a write with `409
parent_archived`; a slug no workspace has is `404 not_found`. `GuardUpdate`
is the same check for a write, the parent read through the update's
transaction.

`internal/fork/update.go` holds the handlers of the fork-only fields
`purpose` and `auto_change` in `PATCH /workspaces/:slug`, both
`workspace.ForkFieldHandler`: `PurposeField` takes `null` (clears the
purpose) or a string of at most 1024 code points, `AutoChangeField` `true`
or `false` and never `null`, any other value being `400 invalid_request`
(`<field>: <reason>`); each returns the write that stores its column in the
update's transaction. `composeForks` registers them with
`(*workspace.Service).RegisterForkField` and `GuardUpdate` with
`RegisterForkUpdateGuard`, so the workspace update judges them after the
other values and asks the guard, for every update of a fork, after the
body is judged and before anything is written. The handler of `ttl` is
`(*fork.Component).TTLField` in `internal/fork/extend.go`, a method because
it needs the hub's clock and the `[forks]` limits, read from the bound
component when it is called: `null`, a number or a boolean is `400
invalid_request` (`ttl: must be a string`), a string is judged by
`ResolveTTL` at the component's `Now` (its `400` answered unchanged), and
the write stores the resulting `expires_at` in the update's transaction, so
that the new lifetime counts from the request, later or earlier than the
one before. The update's `parley.workspace.update` event carries the
workspace's `session_id`.

### Git server

`internal/hub/gitserver.go` adds the part `composeGitServer`, after
`composeForks` in `parts`. It creates a zero `gitserver.Component`, mounts
its three routes with `Composition.Handle`, and binds it in the start hook
`gitserver` to the identity service (`Credentials`), the server's rate
limiter (`Limiter`), the access service (`Guard`, its `GuardGit`, and
`Access`, its `Decide`), `gitserver.StoreRecords` over the store's main
database (`Records`), the fork component (`Parents`, its
`CheckParentState`), `gitserver.DiskRepositories` over the hub's git and
`(*workspace.Service).RepoPath` (`Repositories`), the hub's fork view
(`ForkView`, below), the hub's git (`Runner` and `Copier`), the event
component (`Events`, its `EmitNow`), the workspace service's settings
registry (`Settings`, its `EffectiveSettings`), `[git].timeout` (`Timeout`, read with `gitserver.TimeoutFrom`; the part
registers the key, and `[git].max_push_size`, with `gitserver.Register`) and the hub's logger, after
the functions added with
`AdjustGitServer` have changed those dependencies.
`(*Composition).GitServer()` returns the component. The server, and with it
its limiter, is built after the start hooks, so the component holds a
`serverLimiter`, which a server hook (`addServerHook`) points at
`srv.RateLimiter()` before the first request. The ref writer
(`*pushrule.Writer`) exists from the composition on, so that the forks
part, composed earlier, registers on it, and `(*Composition).RefWriter()`
returns it; the start hook binds it to `rt.Store.Tx`, to the event log
(`c.Events()`), to a head_sha setter that writes `workspace.RecordHead` in
a transaction of its own, to a function that opens a workspace's
repository with the hub's git at `(*workspace.Service).RepoPath`, and to
the hub's logger. The part also registers the push journal's schema steps
(`pushrule.JournalStep`, then `pushrule.JournalEventsStep`) and a second start hook, `push-journal`, after
the `gitserver` hook and so before the hub serves, which runs
`Writer.Reconcile` and fails the start only when the journal cannot be
read. The push rule engine (`*pushrule.Engine`), the push
processor (`*gitserver.Receiver`) and its options stage
(`*gitserver.PushOptionsStage`) also exist from the composition on,
returned by `(*Composition).PushRules()`, `(*Composition).Receiver()` and
`(*Composition).PushOptions()`, so that a part registers its carry-patch
controller, its mapping stage or its session linker while it is composed,
whichever part comes first. The part registers the options stage with
`RegisterOptionsStage`; on it the part that owns the session-linking rule
registers its
`gitserver.SessionLinker` (`RegisterSessionLinker`; the sessions part
registers the session-linking rule, see [Agent sessions](#agent-sessions));
the start hook binds it to the hub's
logger. The start hook binds the engine to
the settings reader `pushSettings`, which reads the effective
`protected_branches` and `protect.owner_push` of the workspace's record,
a fork's from its parent's record, with `workspace.BySlug` and the
settings registry's `EffectiveSettings`, and to the tracking-branch
lookup `trackingBranches`, which asks `(*fork.Component).CheckBranchWrite`
and takes its `tracking_branch` refusal as a tracking branch and any other
error as a failed lookup; it binds the processor to the engine, the ref
writer, the hub's git, `(*workspace.Service).RepoPath` and
`[git].max_push_size` (`MaxPushSize`, read with
`gitserver.MaxPushSizeFrom`), and sets it as
the git server's `Deps.Push` before the `AdjustGitServer` functions run. The
hub's fork view (`forkView` in `internal/hub/gitserver.go`) reads the fork's
record with `fork.BySlug` and names `refs/heads/<b>` for each of its
`TrackingBranches` and `refs/tags/<t>` for each of its `TrackingTags`; it
names as its changes the numbers of every change, in any state, whose
source is the fork (`(*change.Component).ChangeNumbersOfSource`), whose
version refs the fork then shows from its parent's repository. A slug that
names no fork is `fork.ErrNotFork`.

The routes are `GET /git/{org}/{repo}/info/refs`,
`POST /git/{org}/{repo}/git-upload-pack` and
`POST /git/{org}/{repo}/git-receive-pack`; any other path or method under
`/git` gets the dispatcher's `404` or `405` envelope before any credential is
read. `HEAD`, which the router serves with the `GET` pattern, is answered by
the component's handler, before any check and without being counted, with
the same envelope the dispatcher gives a method no route of the path has:
`405 method_not_allowed` with `Allow: GET`. A request on one of them passes these checks in order, and the first
that refuses answers it through the one refusal helper, with
`Content-Type: text/plain; charset=utf-8` and the message followed by one
newline:

1. Authentication: the password of the `Authorization: Basic` header is
   checked with `identity.(*Service).VerifyCredential`; the user name is
   never read. A missing or malformed header (`r.BasicAuth` fails), an empty
   password and every `401` of the check get `401` with
   `WWW-Authenticate: Basic realm="parley"` and `authentication required`;
   a blocked user gets `403 user is blocked` without the challenge; any
   other error is `500 internal error`, logged with the request id, the
   route's pattern and the repository segment. A request that does not
   pass is counted against its client address.
2. The rate limit: the request is counted against its credential
   (`server.LimitKey`), the window the REST API counts the same credential
   in; over the limit it gets `429 rate limit exceeded` with `Retry-After`.
3. The binding guard, for a bound PAT only: `GuardGit` is asked once with
   the caller, the slug as typed (the repository segment without `.git`),
   the method and `r.Pattern`; it reports its own refusals with surface
   `git`. A refusal, and every bound PAT when `Guard` is nil, gets
   `404 repository not found`; an error is `500`. A pass keeps its
   `Binding` in the `Exchange`.
4. The scope, for a PAT only: `identity.Satisfies` with `git:write` when the
   request's need is write, `git:read` otherwise; a failure gets
   `403 token lacks scope <scope>`.
5. Resolution: a repository segment without `.git` gets
   `404 repository not found`; otherwise `Access.Decide` with the need
   decides. `NotFound`, a workspace whose `OrgID` is empty or names no
   organization, and an org segment that differs from the organization's
   slug all get the same `404`; then `ReadOnly` gets `403` with the
   read-only member message, so only a member at the repository's right URL
   learns that it needs a fork. A granted request's workspace record is read
   with `Records.Workspace`. A nil `Access` or `Records`, and every read
   error, is `500`.
6. Validation: on ref discovery the service must be `git-upload-pack` or
   `git-receive-pack`, otherwise `400 invalid or missing service parameter`.
7. State: `workspace.RequireReady` judges the workspace's own record; a
   refusal gets `409 repository not available: workspace is <status>`, with
   `busy` in place of the status when the maintenance marker is set, as
   `RequireReady` reads the marker first. A
   ready task fork is then judged by `Parents.CheckParentState` with
   `fork.UseWrite` for a write need and `fork.UseRead` otherwise: its
   `parent_archived` refusal (a write only) gets
   `409 repository is read-only: parent workspace is archived`, every other
   `409` (a marker, pending, cloning or failed parent) gets
   `409 repository not available: parent workspace is busy`, a `404` (the
   fork removed meanwhile) gets `404 repository not found`, and any other
   error, or a nil `Parents`, is `500`. A repository workspace skips the
   parent check.
8. Opening: `Repositories.Open` opens the workspace's repository and keeps
   its git directory in the `Exchange` (`Repo`); a directory that does not
   open as a repository, and a nil `Repositories`, is `500 internal error`,
   the cause logged, nothing of it in the body.
9. A task fork's refs (`forkview.go`), on all three routes, so that a
   fetch, the version 2 want check and a push all find them up to date:
   `ForkView.ForkRefs` names the fork's tracking refs and the numbers of
   the changes whose source is the fork, the parent's repository is opened
   with `Repositories.Open`, and `Copier.CopyRefs` makes the fork's
   repository agree with the parent's for exactly those refs: each tracking
   ref set to the parent's value or deleted when the parent no longer has
   it, every ref under `refs/parley/changes/<number>/` of a named change
   copied, and every other ref under `refs/parley/changes/` of the fork
   deleted. No other ref of the parent is copied and no other ref of the
   fork, its own branches above all, is touched. The copies are a cache for
   stock git; `(*fork.Component).Refs` stays the one answer to which refs a
   fork has. Two requests on one fork never copy at once: the component
   holds 64 mutexes and a fork's slug picks one (FNV-1a), since two
   concurrent `git fetch` runs into one repository fail on each other's ref
   locks. The 64 locks are channels that hold one token, so a request waits
   for another's copy no longer than its own deadline. The copy runs under
   the request's deadline, `Timeout` after the request came in, which the
   run of git shares, and is logged at debug level when it changed a ref. A nil `ForkView`
   skips the step, so the fork is served with the refs its repository
   holds; a view that fails, a parent repository that does not open, a
   failed copy and a nil `Copier` are `500 internal error`, before git
   runs.

The need is decided from the route before any check runs: write for
`POST …/git-receive-pack` and for ref discovery whose first `service` value
is `git-receive-pack`, read for everything else. A request that passed every
check has an `Exchange`: the route, the git service
it asks for (the route's on the two `POST` routes, the first `service` query
value on ref discovery), the need, the `server.Caller` with the
`identity.Principal` as its `Detail`, the caller's access `Level`, the
guard's `Binding` (nil but for a bound PAT), the workspace record and the
git directory of its repository. A `Service`, when one is set, gets it and
writes the whole answer; otherwise the component serves it (`fetch.go`):

- A push (`POST …/git-receive-pack`) is handed to the `PushProcessor`
  (`push.go`) with `ProcessPush(w, r, PushCaller, workspace.Workspace)`,
  once, with full duplex enabled on the response and the body unread; the
  processor writes the whole answer, honours `Content-Encoding` itself and
  returns the `Result` (`ok`, `refused`, `error`) the request is counted
  with. The `PushCaller` has the credential type (`pat`, `api_key`,
  `admin_token`), the credential id, the user id ("" for the admin token),
  the access `Level` (`owner` or `admin`), the guard's `Binding` and, for a
  PAT, its `identity.Principal` with the paths. With no processor the push
  is answered `500 internal error`, logged, its body unread and git not
  run. The hub's processor is the `Receiver` (`receive.go`, `report.go`),
  which runs no `git receive-pack`: it reads the commands (`<old> <new>
  <ref>`, the capabilities after a NUL on the first) up to their flush
  packet, then the push options when `push-options` was asked for, from
  the body decoded through gzip when `Content-Encoding` says so; a list it
  cannot read is answered `400 invalid receive-pack request`, and one with
  no command `200` with an empty body. Then, in order: the options stage
  (`OptionsStage`, registered with `RegisterOptionsStage`; its answer may
  refuse the push with a message M, the rest of the body read and dropped,
  reported `ng <ref> M` for every ref with one side-band line `error: M`,
  set the session id and mark the updates as opening a change; the hub's
  is `PushOptionsStage`, which refuses with the message of
  `ValidatePushOptions` (`pushoptions.go`: more than 16 options first,
  then the first option at fault in the order sent, `parley.session`
  counting at its place with the linker's answer, so that an option at
  fault before it spares the linker and one after it is judged only once
  the linker accepted it), a session linker's failure
  being the refusal `internal error; try again`, logged, and otherwise sets
  the session the linker accepted and marks the updates when the push has
  `parley.change` or `parley.target` or goes to a task fork whose
  `auto_change` is true); the pack
  into a `gitrepo.Quarantine` (skipped when every command is a delete),
  which fails as `unpack invalid pack` with `ng <ref> unpacker error` for
  every ref when git index-pack refuses it or a new value names no object;
  `Engine.Evaluate` over the quarantine's view, with a fork's parent's
  repository as `ParentRepo`; `Writer.Apply` with `via` `git`, version kind
  `push`, the quarantine's migration as `BeforeCommit` and its undoing
  (`Quarantine.Unmigrate`) as `AfterAbort`; for an accepted
  push the mapping stage (`MappingStage`, registered with
  `RegisterMappingStage`), given the accepted updates with their direct
  flags and versions, whose lines go on the side band (`ChangeMapper`,
  `changemap.go`, is the one the hub's changes part registers: it takes
  each created or updated branch in command order, through a
  `ChangeService`, to one `Outcome`: a version the ref writer reported is
  a new version, with no call; otherwise the branch's draft or open change
  is looked up, and one found was opened after the push moved the branch,
  by a push that won the race to open it, so the push is answered as
  after a lost race (below), whatever it asked; a direct push opens nothing
  and is warned of only when the push has `parley.change` or
  `parley.target`; such a push, or any push to a task fork whose
  `auto_change` is true, opens a change with the push's session; an open
  whose branch was deleted in between returns the change abandoned
  (`OpenedChange.Abandoned`), the reason
  `branch was deleted, change <workspace>#<number> abandoned`; an open
  refused with a `*ChangeExistsError` is a lost race, answered by the
  service's `RecordPushVersion` on the change the refusal named, in one
  store transaction (`change.Component.RecordRacedVersion`): a change
  that left draft and open since is a `*ChangeNotOpenError`, the reason
  `change <workspace>#<number> is <state>`; one that already has the
  pushed head, which the winner's open recorded on following the branch
  as kind `ref` by the hub, makes that version the push's (kind `push`,
  its caller, credential and session) and answers it; one whose branch a
  later write moved on answers its current version and records nothing;
  any other head is recorded as the next version; the `OpenRequest` names the pushed workspace, or a
  pushed fork's parent, as the change's workspace, the pushed workspace
  and branch as its source and the pusher as its caller, and carries
  `parley.target`, `parley.title`, `parley.intent` and `parley.draft`
  when the push gives them, the same for every open of the push, nil
  (or false) leaving the change's defaults; a `VersionRequest` carries
  none of them; before the open, a branch that is its own target (the
  change's workspace is the pushed one and `parley.target`, or else the
  pushed workspace's default branch, a fork's base branch, is the pushed
  branch) is the reason `branch is its own target`, and a PAT without
  `changes:write` (API keys and the admin token are not checked) the
  reason `token lacks scope changes:write`; an open refused with a
  `*server.Error` below 500 is worded by `refusalReason`
  (`pushreport.go`): `target branch <t> does not exist (target_not_found)`,
  `no commits ahead of <t> (nothing_to_land)`,
  `merge commits are not supported (merge_commits_unsupported)`,
  `intent is required; push with -o parley.intent=<text> or -o parley.draft (intent_required)`,
  and any other as `<message> (<error_type>)`; any other failing call, or
  a panic, of the lookup, the open or the record (the hub's service fails
  the open so when, after a `409 change_exists`, its lookup of the
  source's change fails or finds none) is the reason
  `internal error`, logged and never sent, and the next branch is mapped
  all the same; the push is never failed by its mapping.
  `Outcome.Line`, `pushreport.go`, words the lines
  `change <workspace>#<number> version <n>`,
  `change <workspace>#<number> version 1 (opened)` or
  `(opened, draft)`, and `warning: no change for <branch>: <reason>`); for an accepted
  push `Writer.Finish`, which writes its `parley.git.push` events and
  tells the post-push listeners; then the answer.
  The quarantine is discarded on every outcome, after its objects moved
  into the repository for an accepted push; a push stopped once they
  moved in (a failing stage, the store) has them taken out again. The answer is always `200`
  with `Content-Type: application/x-git-receive-pack-result` and
  `Cache-Control: no-cache`: with `report-status` (or
  `report-status-v2`) the status report `unpack ok` and `ok <ref>` or
  `ng <ref> <message>` per command, inside side band 1 with
  `side-band-64k`, which also carries, on band 2 before it, an
  `error: <ref>: <message>` line per refused ref and the mapping stage's
  lines; nothing without `report-status`, and no band-2 line without
  `side-band-64k`. The result is `ok` for an accepted push and a push
  without commands, `refused` for the options stage's, a rule's or the
  compare-and-swap's refusal, and `error` for a pack failure, a timeout and
  an internal error. The pack is read through a `packReader`, which with
  `ReceiverDeps.MaxPushSize` above 0 stops at one byte over it; that push
  fails as `unpack pack exceeds the maximum push size` with
  `ng <ref> unpacker error` for every ref. The git server hands the push
  over with the request's context ending at its deadline, `[git].timeout`
  after it came in, and the processor sets the same deadline on the
  connection's reads, so that a client that stops sending cannot hold it:
  the options stage, the opening of the repository, the pack, the rules and
  `Writer.Apply` up to the preparation of its ref transaction run under
  that context, and when it has ended before the transaction is prepared
  the push's git processes have been killed with it, the quarantine is
  discarded and every ref is reported `unpack push timed out` with
  `ng <ref> unpacker error`. `Apply` runs to its end once the transaction
  is prepared, and the mapping stage and `Finish` run on a context that
  does not end.
- Ref discovery for `git-receive-pack` answers `200` with
  `Content-Type: application/x-git-receive-pack-advertisement` and
  `Cache-Control: no-cache`, always under version 0: the pkt-line
  `# service=git-receive-pack`, a flush packet and the advertisement of
  `Runner.ReceivePack` (`git receive-pack --stateless-rpc --advertise-refs`
  with `receive.advertisePushOptions` and `receive.advertiseAtomic` set), so
  every ref is listed with `report-status`, `delete-refs`,
  `side-band-64k`, `ofs-delta`, `atomic` and `push-options`.
- Ref discovery for `git-upload-pack` answers `200` with
  `Content-Type: application/x-git-upload-pack-advertisement` and
  `Cache-Control: no-cache`; under version 0 the body is the pkt-line
  `# service=git-upload-pack`, a flush packet and git's advertisement,
  under version 2 git's capability advertisement alone. Version 2 is
  chosen when an entry of the `Git-Protocol` header, split at `:`, is
  `version=2`.
- `POST …/git-upload-pack` answers `200` with
  `Content-Type: application/x-git-upload-pack-result` and
  `Cache-Control: no-cache`, enables full duplex on the response (git reads
  the body while the answer goes out) and gives git the body, through a
  gzip reader when `Content-Encoding` is `gzip` or `x-gzip`. An empty body
  is answered with the pkt-line `ERR empty request`, a body that is not
  valid gzip with `ERR invalid gzip body`, and git does not run.
- The run goes through `Runner.UploadPack` under the request's deadline,
  `Timeout` (zero: `DefaultTimeout`) after the request came in, of which a
  fork's ref copy has already used its part. Every write of git goes to
  the client at once and is flushed, through a `pktScanner` that records
  whether pack data (a `packfile` section, a payload starting with `PACK`
  inside or outside the side band) or an error packet (`ERR …` or the side
  band's channel 3) went by. When git fails or the timeout ends it before
  pack data and with no error packet of git's, the answer ends with one
  `ERR` pkt-line: `request timed out` for the timeout, `request too large`
  for a version 2 request beyond `gitrepo.DefaultMaxRequest`, otherwise the
  last line of git's standard error without its `fatal: ` or `error: `
  prefix and with the repository's directory left out (`upload-pack
  failed`, or `receive-pack failed` on push ref discovery, when git wrote
  none, or when the line still names an absolute path, such as the object
  directory of a fork's parent). The
  failure is logged at warn level with the request id,
  route and repository segment. A client that went away is told nothing;
  its context's end kills git.
- Every run, ref discovery and fetch alike, gets the same configuration on
  git's command line (`uploadPackConfig`): `uploadpack.allowFilter=true`
  with `uploadpackfilter.allow=false` and only `blob:none`, `blob:limit`
  and `tree` allowed, so that git itself refuses every other filter with
  an `ERR` packet, and `uploadpack.allowReachableSHA1InWant=true`, which
  advertises `allow-reachable-sha1-in-want` under version 0, so that a
  client asks for a commit by id. No ref is hidden. Shallow, incremental
  and partial fetches are git's own.
- git serves a tree, a blob or a tag by its id under either version, and
  under version 2 a commit too, whether a ref reaches it or not, whatever
  its configuration. `gitrepo.UploadPack` therefore checks every want of a
  fetch before upload-pack runs. It keeps the request in a file of git's
  private directory, a version 2 request whole, a version 0 request up to
  the flush packet that ends its wants, git reading the rest as it comes
  (read under the request's context, so a client that stops sending is
  held no longer than the timeout, and up to `DefaultMaxRequest`, 16 MiB,
  beyond which it is refused with `ErrRequestTooLarge`, git not run). It
  reads a version 2 request as git's version 2 server does: the command and
  the capabilities in any order up to the delimiter packet, then, for a
  `command=fetch`, the `want` lines; a version 0 request's `want` lines come
  first. Each id is read as git reads it, the leading 40 (SHA-1) or 64
  (SHA-256) hexadecimal digits in either case, lowercased, whatever follows
  them; a version 2 want without at least 40 such digits is refused with
  git's text `upload-pack: protocol error, expected to get oid`. It runs
  `git rev-list --ignore-missing --stdin --not --all` over the wanted ids:
  a commit it lists, an annotated tag's commit included, is reached by no
  ref. Then `git cat-file --batch-check` gives each id's type, and each
  tree, blob or tag that no ref points at (`git for-each-ref`) is looked for
  among the objects the refs reach, `git rev-list --objects
  --in-commit-order --use-bitmap-index --all`, read as it comes and stopped
  once every one has been met. A want of an object no ref reaches is
  refused before upload-pack runs with a `*ServeError` of git's version 0
  text, `upload-pack: not our ref <id>`, which the component frames as its
  `ERR` pkt-line. An id the repository does not resolve (an object it does
  not hold, an id of the other object format) is left out and refused by
  git itself. A fork's refs are its own, so an object of the parent that
  the fork reads only through its alternates is refused to it (see
  [errata/17_git_smart_http.md](errata/17_git_smart_http.md)).

The fetch side never reads the `paths` of a PAT.

Every request on the three routes is counted once (`metrics.go`): the
handler wraps the response writer in a `statusWriter`, which records the
status and unwraps to the server's writer, and counts the request before it
returns, so before the server writes the end of the body and a client that
has read its answer finds it counted. The series is `RequestLabels{Service,
Result}`, the service label taken from the route (`info-refs`,
`upload-pack`, `receive-pack`). An exchange with git reports its own result:
`ok` when git succeeded, `error` for an `ERR` of the hub's (empty request,
invalid gzip, request too large), a failed git, a timeout or a client that went away; a push
reports the processor's `Result`. Every other answer, a refusal of a check or
the answer of a `Service`, is judged by its status: `refused` for `401`,
`403` and `404`, `ok` below `300`, `error` otherwise. `Component.Requests`
returns a copy of the counts; nothing exports them at `/metrics` yet.

The fetch exchange passes git's request through a `requestScanner`
(`pktline.go`) as git reads it, under either protocol version and whether
git streams it or `gitrepo` first keeps a version 2 request in a file. The
scanner shares its pkt-line framing (`pktFramer`) with the `pktScanner` of
the answer and notes a `deepen`, `deepen-since` or `deepen-not` line and the
text of a `filter` line (up to 1024 bytes of it). After a fetch whose result
is `ok` and whose answer carried pack data, `recordFetch` (`event.go`) reads
`audit_fetches` from `Settings.EffectiveSettings` of the workspace record's
settings document and default branch, a task fork's parent's record read
with `Records.Workspace` in its place (false without `Settings`, while the
key is not registered, and for a fork whose parent is not found or cannot be
read) and, when it is true, writes
`parley.git.fetch` with `Events.EmitNow` under the request's context
without its cancellation: workspace and resource id the addressed slug,
resource type `workspace`, actor type the credential's (`admin_token` for
the admin token, no actor id), the user as actor id for a PAT or an API
key, metadata `{"shallow": bool, "filter": string or null}`. A nil `Events`
writes nothing; an error is logged at error level with
`"event":"parley.git.fetch"` and changes neither the answer nor the count.

### Source control reads

`internal/hub/scm.go` adds the part `composeSCM`, after `composeGitServer`
in `parts`. It creates a zero `scm.Component`, registers its
`Routes()` with `AddRoute`, and binds it in the start hook `scm` to
`workspace.BySlug` over the store's main database (`Workspaces`), the fork
component's `CheckParentState(ctx, slug, fork.UseRead)` (`ParentState`)
and `CheckParentState(ctx, slug, fork.UseWrite)` (`WriteParentState`),
the hub's git opening `(*workspace.Service).RepoPath(slug)` (`Open`), the
ref view (`RefView`: for a task fork `forkRefs`, an adapter over
`(*fork.Component).Refs` that names each branch `refs/heads/<name>` with its
`Tracking` flag and each recorded tag `refs/tags/<name>`, left for the
reads to peel, and no `refs/parley/` ref; for a repository workspace
`scm.RepoRefs`, every ref of its repository) and `protectedBranches`
(`Protected`), `pushrule.Protected` over the effective `protected_branches`
the push rule engine's settings reader returns (a fork's are its
parent's), with `ReadTimeout` and `MaxFileSize` from `Config.SCM`
(`[scm].read_timeout`, `[scm].max_file_size`), `Now` the hub's clock
(`Runtime.Now`), and to the push rule
engine (`Rules`, `(*Composition).PushRules()`) and the ref writer
(`RefWriter`, `(*Composition).RefWriter()`) of the git server's part, the
event log (`Events`, `(*Composition).Events()`) and the hub's logger
(`Logger`), after the functions added with `AdjustSCM` have changed those
dependencies. `(*Composition).SCM()` returns the component. The change
part sets `Changes` (`OpenChangeOfBranch`, `ChangeOfCommit`) through
`AdjustSCM` and registers the `ChangeLinker` (see [Changes](#changes)).
The land part sets `Attribute` through `AdjustSCM` to
`(*land.Component).Attribute`, which the comparison and the server-side
rebase call on their conflict report (an error is logged and the report
answered without it), and `Sessions` (`SessionOf`) to
`commitSessions` (`internal/hub/land.go`), which answers from the records
that hold sessions: for the head of a change's version, the session of
the first version with that head (`(*change.Component).SessionOfHead`);
for a commit a land wrote in place of a version's commit
(`ChangeOfCommit`, `Landed` with a `LandedFrom` other than the commit),
the session of the attempt that landed the change
(`(*land.Component).SessionOfLanding`); none for any other commit.
It sets `change.Deps.Attribute` to the same function through
`AdjustChange`, which the mergeability check calls on the report of its
trial, outside any transaction, and a restack on the report of its replay,
before its transaction; `internal/change` and `internal/scm` cannot import
`internal/land`, so they take the attribution as a function value, and nil
names no landed change.
`Recorder` keeps its default, which records nothing, since the metric
export is not in the hub yet; the sessions part registers the
`SessionLinker` (see [Agent sessions](#agent-sessions)).

Every read route is `GET /workspaces/:slug/scm/…`, declared with the scope
`git:read` and the access need `read`, so the pipeline answers `401`,
`403` `insufficient_scope` and `404` `not_found` before the handler. One
wrapper around every handler reports the request to `Deps.Recorder`
(`Observe(op, result)`): the route's op and `scm.ResultOK` when the status
written, or the status of the error returned before anything was written,
is below 400, `scm.ResultError` otherwise. The same wrapper gives the
handler's request a context that ends at `Deps.ReadTimeout`, with a cause
of its own that tells the deadline from a client that went away; every git
command of the read runs under it, and the git runner kills the process
group and waits for it when it ends. Its response writer passes nothing on
once the deadline has passed: when the handler had written nothing, the
wrapper restores the headers it found and returns `503 scm_timeout`, also
when the handler's git work finished in time and only the answer was
late; when the answer had begun and the handler failed or wrote after the
deadline, it panics with `http.ErrAbortHandler`, so net/http drops the
connection rather than end the body as if it were whole, and reports the
request as `scm.ResultError`. A handler checks its query
first, then begins the read: the record (`Workspaces`), the state check
(`ready`, or `archived` with no maintenance marker, passes; anything else
is `workspace.RequireReady`'s refusal), `ParentState`, and only then
`Open` and the workspace's ref view, with a `Resolver` over both. So a
refused read opens no repository. The protected-branch test is asked once
per read; a `refs/parley/` ref is protected whatever it says. A commit
listing's cursor holds `{sha, offset}`, the commit the first page resolved
and the commits the pages before held, so a later page skips resolution and
reads the history of that commit, also after its ref moved. A tree
listing's cursor holds `{sha, path}`, the commit the first page resolved
and the last path written, so a later page lists the same tree after that
path: `ReadTree` gets the path as `TreeRead.After` and one entry more than
the page as `TreeRead.Limit`, and reads no directory before the path nor
past the page. Every route resolves each revision once, at the start, through the
`Resolver`, which reads the ref view once; everything after it reads only
commit, tree and blob ids, so a ref that moves during a read cannot mix two
commits into one answer. Right after resolution, and before any other git
work, each route calls `notModified` (`cache.go`): it sets an `ETag`, the
quoted sha256 digest of the request's path, raw query and the parts the
route gives (the resolved commit ids, and `raw`/`json` on the file route,
`json`/`patch` on the diff route; the refs routes give the JSON of their
ref objects), and `Cache-Control` `private, max-age=31536000, immutable`
when every revision was written as a full id (`Resolved.Full`; on a later
list page, the `ref` of the query) or `private, no-cache` otherwise, and
answers `304` itself when `If-None-Match` holds the tag, so a current copy
runs no diff, blame or search. The handler wrapper restores the
`Cache-Control` and `Vary` the server set, and drops the `ETag`, when the
handler wrote nothing, so an error envelope carries none of them. The file route looks the file up
(`LookupFile`), compares its size with `Deps.MaxFileSize` before reading
any of it, and streams the blob (`WriteBlob`) through a writer that sends
the headers with the first byte, so that a failure before it is still
answered with the error envelope. The diff route resolves `base` and
`head`, asks for their merge base (`MergeBase`), lists the file changes
(`DiffChanges`) from it, or from `base` with `mode=direct`, keeps those a
`path` glob matches, counts `stats` over all of them and lists the first
5,000; it then reads the patches through `DiffPatches`, which streams git's
patch and hands each file's part over by the index of its change, adding
each kept file's part until the next would pass 8 MiB, and stops git there,
so neither the diff's whole patch nor more than 8 MiB of it is held.
The compare route resolves `base` and `head` and first tries the rebase
(`TrialRebase`), which refuses a merge commit among the commits to replay
before replaying any, reads no commit's message, and writes objects but
moves no ref; then it reads the
merge base, the counts (`AheadBehind`), the first 200 commits only `head`
has (`CommitsBetween`, `ReadCommits`, commit objects without `stats`) and
the first 5,000 file changes from the merge base to `head` (`DiffChanges`,
the empty tree when there is none). It needs git 2.40 or later, the first
whose `merge-tree` takes `--merge-base`. The replay (`replay`,
`rebase.go`), which `Rebase`, `RebaseFrom`, `RebaseFromAs`, `TrialRebase`
and `TrialRebaseFrom` share, merges each commit with `merge-tree
--write-tree --messages`; a conflict is parsed from its stage entries and
messages (`parseConflicts`), a side git moved aside to `<path>~<commit>`
is given back its own path (`asidePath`), the files are sorted by path, and
each gets one kind (`conflictKind`), the first that applies of submodule,
binary, rename, add_add (no base and different content, or git's
`CONFLICT (file/directory)`), modify_delete, delete_modify, mode (git's
`CONFLICT (distinct modes)`, or two modes on ours and theirs that do not
merge, a mode one side alone changed merging, with the same content or
content that merges) and content, the target being ours and the commit theirs; a
file's path is ours' path where ours holds it (for a rename, the target's
new path). `replay` takes a resolver
(`resolveFunc`, nil for the trial and every rebase but
`RebaseFromResolving`), asked with the tree `merge-tree` wrote for a
commit that does not apply (`mergeTree` returns it with the files).
`RebaseFromResolving`'s resolver (`resolver`, `conflict.go`) gives a
tree only when every conflicted file's path has a resolution: each
resolution's blob is written once (`git hash-object -w --stdin`), and
`editTree` rewrites, with `git ls-tree -z` and `git mktree -z --missing`
in the object store, each directory on the way to a resolved path, putting
the blob at the path (in place of a directory too) or removing the file
there, and removing the sides moved aside to `<path>~<commit>` and the
paths the file's other sides name (a rename's base and theirs paths); the mode
is the theirs side's, else ours', when a file mode, else `100644`.
The blame route looks the file up (`LookupFile`), refuses a file over 2 MiB
before reading it, reads it (`WriteBlob`) to refuse a binary one (a NUL
byte in its first 8,000 bytes, as git decides) and to count its lines, which
check `start_line` and cut `end_line`; only then does it run `Blame` for that
span, and it reads the distinct commits of the ranges in one `ReadCommits`,
so each commit's object, and its lookups, are built once per request.
The search route's cursor holds `{sha, path, line}`, the commit the first
page resolved and the last match written; a later page searches that commit
again (`Grep`) from that position (`GrepQuery.AfterPath`, `AfterLine`), so
git searches no file before it, keeps the first 500 characters of each line
(`GrepQuery.MaxText`) without holding the rest, and stops git once it has
read one match past the page, so no search holds more than a page and one
match.

### Source control writes

Every write route of `internal/scm` is built by one wrapper,
`writeRoute(method, pattern, op, h)` in `write.go`: the route at
`/workspaces/:slug/scm` plus `pattern`, declared with the scope `git:write`
and the access need `write`, so the pipeline answers `401`, `403`
`insufficient_scope` (the same whether or not the workspace exists), `404`
`not_found` to a caller with no access and `403` `forbidden` to an
organization member, who has read access only, before the body is looked
at; the owner and the admin token pass. The wrapper then reads the
workspace's record once (`Workspaces`) and refuses it by its state with
`workspace.RequireReady` (`409` `workspace_archived`, `workspace_busy`
while it is `pending` or `cloning` or a reclone, archive or delete runs on
it, `workspace_not_ready` once its clone failed), and for a task fork by
its parent's with `Deps.WriteParentState`, which the hub binds to the fork
component's `CheckParentState(ctx, slug, fork.UseWrite)` (`409`
`parent_archived`, `workspace_busy`, `workspace_not_ready`). No repository
is opened before these checks pass. The handler gets one value holding the
caller's `identity.Principal`, the access check's `access.Decision` (level
and record), the binding of a bound PAT and the workspace's record, so it
looks none of them up again. The wrapper reports each request the handler
answers to `Deps.Recorder` under the route's op (`commit`, `ref_create`,
`ref_move`, `ref_delete`, `rebase`): `scm.ResultRefused` for a refusal of a push
rule, `scm.ResultRefMoved` for a `409` `ref_moved`, otherwise
`scm.ResultOK` for a status below 400 and `scm.ResultError` from 400 on;
a request the shared checks or the state checks refuse is not reported.
Writes have no read timeout.

Every write moves its ref through one step, `updateRef` in
`refupdate.go`, so that the REST API and the git server are two doors to
one repository: it builds the caller's and the workspace's facts as the
git server does for a push (`pushrule.Caller` with the access level, the
credential type, `admin_token` for the admin token, its id, the user, the
binding of a bound PAT and a path-scoped PAT's paths; `pushrule.Workspace`
from the record; for a task fork the parent's git directory as
`ParentRepo`), asks `Rules.Evaluate`, and, when the update is allowed,
`RefWriter.Apply` with `via` `api`, the version kind the route names
(`ref` for a create or a move, none for a delete) and the session, then
`RefWriter.Finish` with a context that is not cancelled, which writes the
`parley.git.push` events and tells the post-push listeners. A refused
update is `403` with the rule's message and error type (`500` `internal`
with a fixed message for a rule that could not be evaluated), with the
refused path as the error's member `path` for `path_not_allowed` and the
fork record's `ForkPoint.Ref` as `details.base_ref` for `tracking_branch`;
a refusal returns before `Apply`, so no ref moves, no event is written and
no change is opened. A write reads the ref it moves through the target's
`lookup`: the ref of the workspace's repository, else, in a task fork, the
tracking branch of the fork's ref view, so that a write to a tracking branch
reaches the push rules with the parent's commit as its before value and is
refused there, as a push of it is. An `Apply` whose compare-and-swap
failed `409` `ref_moved` with `details.current_sha` read after the
failure, and a ref-updated stage's error is answered as it is when it is
an API error, else `500` `internal`. The step returns the engine's decision
(its `Direct`, `Created`, `Forced` flags) and the version a stage recorded.
The owner exemption is the engine's: a fast-forward of a protected branch
by the owner or the admin token with an exempt credential comes back with
`Direct` set, and the route then asks the `ChangeLinker` nothing and opens
no change; it names the version a stage recorded (`refUpdated.change()`),
which it records only when the protected branch is already the source of a
draft or open change.
A write opens the workspace's repository through `Deps.Open`, which must
hand over a `*gitrepo.Repo`, since the ref writer writes to one.

The refs write routes take a full ref name or a branch name; a name must
pass `gitrepo.ValidRefName` and lie below `refs/heads/` or `refs/tags/`, a
name below `refs/parley/` being handed on to the rules, which refuse it.
They read the ref's current value through the workspace's ref view
(`(*target).lookup`: the repository's own ref, or in a task fork a tracking
branch at its parent's commit), check `expected_sha` against the commit it reaches (`409`
`ref_moved` before the rules are asked), and pass that value as the
update's before, so a write that races another loses at the ref writer's
compare-and-swap. A delete asks the registered `ChangeLinker` whether the
branch is the source of a draft or open change (`409` `ref_in_use`);
without one (`noLinker`), no branch is, and every opening is refused with
`400` `invalid_request`.

The commit route (`createCommit` in `commit.go`) checks the body and every
operation's shape first (`gitrepo.ValidPath` for each path), before it
opens the repository; then refuses a `change` object on a branch the
workspace protects (`400` `invalid_request`), asking `Deps.Protected`, the
push rules' protected-branch test over the effective `protected_branches`
the engine reads, before any object is written; then resolves the parent: for an existing branch
its head through `(*target).lookup`, which `expected_head_sha` must name (`409`
`ref_moved` otherwise, also when it is absent), for a new one the commit
`base_ref` names through the revision resolver, before `pushrule.ZeroID`;
then `ApplyOperations` (an `*gitrepo.OperationError` is `400`
`invalid_operation` with `details.index`), compares the tree with the
parent's (`400` `empty_commit` unless `allow_empty`), writes the commit
with `CreateCommit` (the request's author, else the caller's full name or
username and e-mail, the hub for the admin token; committer
`gitrepo.HubSignature`; both at `Deps.Now()`), and moves the branch
through `updateRef` with the version kind `commit` and the parent as the
before, so of concurrent commits on one head only the first to reach the
ref writer's compare-and-swap wins. Its answer is the commit object with
`stats` and the ref object. The commit route is the one route whose body
limit is set after the composition: `composeSCM` registers it with
`scm.DefaultMaxCommitSize`, and its start hook, which runs after the
configuration is loaded and before the server installs the routes, sets
it to `[scm].max_commit_size` with `scm.LimitCommitBody`.

The rebase route (`rebase` in `rebase.go`) checks the body first (`branch`
through the same branch-name check as the commit route, `onto` present,
`expected_head_sha` a full id), then reads the branch's head through
`(*target).lookup` (`404` `ref_not_found`; `409` `ref_moved` when
`expected_head_sha` names another commit) and resolves `onto` with the
revision resolver. When `onto` is the head or an ancestor of it
(`gitrepo.IsAncestor`) it answers `up_to_date` with the change the
registered `ChangeLinker`'s `FindOpen` names, and asks neither the rules
nor the ref writer. Otherwise `gitrepo.Rebase` replays the commits at
`Deps.Now()` (`ErrMergeCommit` is `400` `merge_commits_unsupported`, a
`*gitrepo.ConflictReport` `409` `rebase_conflict` with the report as
`details`), and the branch moves through `updateRef` with the version kind
`rebase` and the head read at the start as the before, so a branch moved
while the commits were replayed is `409` `ref_moved`. The answer's `change`
is the version a ref-updated stage reported for the update. Between the
replay and `updateRef` the component runs the hook `replayed`, which only
a test sets (`export_test.go`), to move the branch in between.

The commit and the rebase route judge a `session_id` right after the body
(`checkSession` in `session.go`): the registered `SessionLinker`'s
`Check(ctx, caller, sessionID, slug)` gives the id back in lower case, an
error matching `scm.ErrUnknownSession` is `400` `unknown_session` and any
other error `500` `internal`; without a registered linker every id is
`400` `unknown_session`. The check only judges: the session goes with the
update to `Apply` (`ApplyRequest.SessionID`), so the ref-updated stages,
the `parley.git.push` events and the post-push listeners all have it, and
the link of a write to a session is the version a stage records with it.
Like the change linker, it is registered on the component instance, not
in a package registry (ADR 08). The routes call no post-push listener and
keep no registry of them: `Finish`, the ref writer's event step, tells the
listeners registered on the ref writer of every accepted write, `via`
`api` included.

The commit route builds its commit's `parley.scm.commit` event before the
branch moves and hands it to `Apply` as `ApplyRequest.Events`
(`refUpdate.events`): the ref writer journals it with the request, and
`Finish` writes it after the push event in the same store transaction, or
`Reconcile` at the next start, so a hub that stops once the branch has
moved still writes both. The event has the caller as actor,
`resource_type` `commit`, the session, and metadata `branch`, `sha`,
`parent_sha` and `files` (`gitrepo.ChangedPaths` against the parent, kept
with `event.PathList`; paths that cannot be read are logged through
`Deps.Logger` and left out). `refUpdate.beforeWrite` runs once the rules
allowed the update and before `Apply`: the commit route asks the
`ChangeLinker`'s `CheckOpening` there for a `change` object, whose error is
the answer with nothing moved, and opens the change with `Open` after the
branch moved. Without a `change` object, in a task fork whose
`auto_change` is on, a branch for which no stage recorded a version and
`FindOpen` names no draft or open change gets `Open` in the parent with
the fork's `ForkPoint.Ref` as target; a failure of either opening after
the move keeps the commit and is the answer's `change_error`
(`{error_type, message}`, an API error's own, else `internal`). A direct
push calls no linker. The three `POST` routes are registered with
`server.Route.Create`, so the server's idempotency stage replays them.

### Changes

`internal/hub/change.go` adds the part `composeChanges`, after `composePolicy`
in `parts`. It creates a zero `change.Component` (`Changes()`), adds
`change.SchemaStep` (the step `changes`) and right after it
`change.AutoLandSetterStep` (`change_auto_land_setter`),
`change.MergeableStep` (`change_mergeable`), `change.AnnouncedStep`
(`change_ready_announced`), `change.StackStep` (`change_stack`, the
column `restack_onto`: the commit a change must sit on once the change
below it landed, `NULL` for none) and `change.RestackStep`
(`change_restacks`, the table of pending restacks), registers its `Routes()` with
`AddRoute`, and binds it in the start hook `changes` to the store
(`Store`), the hub's git opening `(*workspace.Service).RepoPath(slug)`
(`Repos`), the access service (`Access`), `changeWorkspaces` (`Workspaces`), an adapter over
`workspace.BySlug`, read through the querier it is given, so in the
caller's transaction, with `workspace.RequireReady` as `Unusable` and the
effective `changes.require_intent`, the event component (`Events`), the
hub's clock and logger, and `[changes].mergeable_debounce`
(`MergeableDebounce`); `AdjustChange` adjusters run before the bind. Right
after it, the start hook `changes-reconcile` runs `Reconcile` (below), so
it runs after the `push-journal` hook and before the listener is bound. It
adds the background work `change_mergeability` (grace 5 s), which runs
`RunMergeability` until the stop, and `change_restack` (grace 5 s), which
runs `RunRestacks` (below).

The schema keeps `changes` (one row per change, keyed by target workspace
and number, with the current version's head, base, stats, touched paths
and patch id copied onto it, the abandon reason, and lists and objects as
JSON), `change_versions` (keyed by change and version, with the kind, the
patch id, `patch_equivalent_to`, `created_by`, the creating credential's
type and id, the session and the stats), `change_sessions` (the order of
each session's first version), `change_landed_commits`
(`version_sha`, `landed_sha`) and `change_counters` (the last number given
per workspace, so a number is never given twice while the workspace
exists). The three child tables
cascade from `changes`; a unique partial index keeps one draft or open
change per source branch, and a violation answers `409 change_exists`.

`(*change.Component).Open(ctx, tx, change.OpenInput)` runs in the caller's
transaction and neither begins nor commits one. It first runs every check
of an open, in order and writing nothing (the unexported `check`, which
reads through a `Querier`): the binding of a bound token, the input
(`source.branch`, at most `MaxLabels` labels matching the label pattern,
a positive `depends_on`), an explicit session, the caller's access, the
target's and source's `Unusable`, then the target branch, the integration
branch of a carry-patch target, the source being the target or a fork of
it, the source branch, not the same branch, a commit ahead, no merge
commit ahead, no draft or open change of the source, the title and the
intent, and, with `CheckPaths` (which the route sets), every touched file
against a path-scoped caller's globs (`identity.MatchAny`). With
`SourceGranted`, which only the revert sets once it has decided who may
revert, the caller's access asks read access to the target and not write
access to the source, since the revert's branch lives in the target and
an author landing by `land.by_author` may only read it. Binding and
access are `(*change.Component).Authorize(ctx, caller, target, source)`
(`authorize.go`), exported for the other surfaces that open changes: it
asks the access rule (`change.AccessRule`, the hub's `*access.Service`)
for the bound token's `RelationOf` the target and source, the caller's
`Level` on the target and `CanWrite` on the source, telling a source the
caller reads (`403 forbidden`) from one they cannot see (`400
source_not_related`); the admin token and the hub pass. A bound token's
workspace reaches it as `change.Caller.Bound`, from the principal. It
then reads the target and
source workspaces, chooses the session (an explicit one the session linker
must accept; else the trigger's, then the source fork's, dropped when
refused), resolves the target and source branches, and measures the
version in the source workspace's repository, which for a fork borrows the
parent's objects: the merge base with the target branch, the file changes
and stats, the touched paths and the patch id (`patchID` in `diff.go`: a
SHA-256 over the path and the patch key of each file of the base-to-head
diff, in path order, the keys the interdiff compares, so that two versions
have the same patch id exactly when their interdiff changes no file;
`empty` for a diff that changes nothing). It then takes the next number, inserts the change and version 1,
writes `refs/parley/changes/<number>/1` in the target's repository
(`SetRef`, or `SetRefFrom`, which pushes a fork's commit into the parent
first), records the session link, asks the `AutoLandGuard` and stores
`auto_land` through `SetAutoLand`, runs the `StackLinker`'s `LinkVersion`,
the `LandingEvaluator` (for a draft too, given the cause `draft`) whose answer it stores, and the
`VersionObserver`s, each with the change re-read in the transaction, tells
the `ForkActivity` recorder of a fork source (an error is logged), and
emits `parley.change.version` and, unless draft, `parley.change.open`. The
version ref is written before the collaborators run, which may read the
head from the target's repository, and before `Open` returns, since a git
ref cannot join the transaction. When a step after it fails, `Open`
removes the ref again (`gitrepo.(*Repo).DeleteRef`; a failed removal is
logged), so a refused open leaves no ref; a ref left by a transaction the
caller rolls back after `Open` succeeded names no stored version and is
overwritten when the number is next recorded.

`(*change.Component).RecordVersion(ctx, tx, change.VersionInput{Workspace,
Branch, Head, Kind, Caller, SessionID})` (`version.go`) records a move of a
branch in the caller's transaction too. `Workspace` is the source
workspace, whose branch moved; the change is the draft or open one of that
source branch (`activeOfSource`), which lives in the target, the parent of
a fork. With no such change, or a `Head` equal to the change's head, it
returns no version and no error. A `SessionID` must pass the session
linker (with none registered, every session is refused) or the answer is
`400 unknown_session`, before anything is written. It measures the head as
`Open` does, in the source's repository: the merge base with the target
branch, or the previous version's base while the target branch does not
exist, the stats, the touched paths and the patch id, and sets
`patch_equivalent_to` to the latest earlier version with the same patch id.
It inserts the version with the creating credential (`Caller.Credential`;
the zero `Caller` is the hub, actor type `system`, no credential), writes
`refs/parley/changes/<number>/<version>` as `Open` does (and removes it
again when a later step fails), records the
session link, makes it the change's current version (head, base, patch id,
stats, paths, `session_id`, `mergeable` `unknown`, `updated_at`), then runs
the stack linker, the landing derivation and the observers as at open, and
emits `parley.change.version` with the version re-read from the
transaction (`storedVersion`), since the stack linker may have moved its
base, and so its stats and `patch_equivalent_to`; `Open` does the same.
A draft has the cause `draft`. A missing target branch, a head with no
commit the target lacks, a head with no history in common with the target
and a merge commit among the version's commits have the causes
`target_not_found`, `nothing_to_land`, `unrelated_history` and
`merge_commits`, which `moveCause` decides and `measureMove` returns with
the measurement. A registered evaluator is
asked in every case with that cause (`EvaluateLanding(ctx, tx, ch, v,
cause)`), and its answer, blocked for the cause with the requirements
still listed, is stored. Without one, a cause stores `blocked` with it and
no requirements, a draft's landing is left as it is, and any other change
is `ready`. The change keeps its state.
A version with no merge base has `BaseSHA` nil (stored as the empty
string) and is measured from the empty tree, every commit of its head
counted.

The collaborators (`internal/change/collaborators.go`) are registered on
the component instance, with `RegisterSessionLinker`,
`RegisterStackLinker`, `RegisterLandingEvaluator`, `AddVersionObserver`,
`RegisterAutoLandGuard`, `RegisterForkActivity` and, declared beside the
abandon in `lifecycle.go`, `RegisterRestacker`
([ADR 08](adr/08-register-the-push-collaborators-on-the-ref-writer.md)).
The hub registers the fork activity recorder (below) and the component's
own stack linker (`StackLinker()`) and restacker (`Restacker()`), both the
type `stacks` (below), and no session linker or auto_land guard; the
landing policy's part registers the landing evaluator (see
[Landing policy](#landing-policy)).

The stack linker (`stack.go`, the type `stacks`) links each new version,
at open and after, from the commit graph: of the other draft and open
changes of the same workspace and target branch, whose head is not the
new head, it keeps those whose head is a proper ancestor of the new head
(`IsAncestor`, asked of the target's repository, which holds every
version head of its changes under their version refs, a fork's too), and
takes the one with the fewest commits to the new head (`CommitsBetween`),
the lowest number on a tie. It stores the link through `SetDependsOn` when
it differs and, for a link, moves the version's base to that change's head
through `SetBase`, so that the stats, paths and patch id count only the
change's own commits; with none below, it clears the link and the version
keeps the merge base with the target branch that measuring it found. The
`depends_on` an open names is the link of version 1 instead (later
versions are linked from the commit graph); `SetDependsOn` (for `PATCH`)
links to the number given the same way, or, for `null`, clears the link
and moves the base to the merge base of the head and the target branch's
head (kept while the target branch is missing), then schedules the
change's mergeability check; the link the change already has writes
nothing. An explicit link is checked first by `checkDependency`, one
function for both: a number that names no change of the workspace, the
change itself, a change into another target branch, one neither draft nor
open, or one whose chain of `depends_on` links, walked down, reaches the
change is `400 invalid_dependency`, which rolls back the open's or the
`PATCH`'s transaction. An accepted link moves the base to the lower
change's head only when the head is built on it (`IsAncestor`); otherwise
the base stays and `linkTo` calls `pendRestack` with `TriggerManual`
(`manual`). After every link it stores (`LinkVersion` and
`SetDependsOn` alike), `recomputeDepth` counts the depth of the change
(`depthOf`: the changes in its chain of `depends_on` links down to one
with none, itself included) and of every draft or open change above it
(`stackAbove`, each one more than the change it depends on), and sets
`needs_restack` with `restack_reason` `stack_too_deep` through
`SetNeedsRestack` on each one deeper than `Deps.MaxStackDepth` (from
`[changes].max_stack_depth`, `DefaultMaxStackDepth` 20 when zero), and
clears both on a change so flagged that is back within the limit
(`unflagDeep`), which then, unless the change has a pending restack
already, calls `pendRestack` with `TriggerVersion` when the change is
`behind`: `sitsOn` in the target's repository says its head and base do
not sit on its `sitOn` commit. Depth never refuses a version or a link.

Then `settle` clears `needs_restack` and `restack_reason`, and
`restack_onto`, of a change flagged for `conflict` or `auto_restack_off`
whose new head has the commit it must sit on (`sitOn`, then `IsAncestor`
in the target's repository): the head of the change it now depends on,
or, unlinked, its `restack_onto`, else the head of its target branch (a
missing target leaves the flag); a change the version unlinked is cleared
without one, since it has no change below it to sit on. Other flags are
left. Every clearing of a `conflict` flag (`clearNeedsRestack`, in
`conflict.go`: `settle`, a restack's version, an up-to-date restack) also
sets `mergeable_last` from `conflict` to NULL, so that the next conflict
of a restack, stored before a check computed the change's mergeability
again, is a transition and writes `parley.change.conflict`.

`renewRestack` then records the change's own pending restack again, when
it has one and the version is not of kind `restack` (whose restack
removes the record): `INSERT OR REPLACE … SELECT` gives it a higher
`seq`, so a record the worker marked failed runs again on the change's
own version, and ends either way (a change pushed onto its commit is up
to date), after which its mergeability check is scheduled.

Last, `LinkVersion` records a pending restack (`pendRestack`, trigger
`version`) of each direct dependant of the change, one already on the new
head too (its restack is up to date and clears a stale `conflict` or
`auto_restack_off` flag), in the version's transaction, so that an error
writing it undoes the version.
`pendRestack(ctx, tx, ch, trigger, onto)` is the one writer of a pending
restack: `INSERT OR REPLACE` into `change_restacks` (`seq` `INTEGER
PRIMARY KEY AUTOINCREMENT`, `workspace_slug`, `number`, `trigger`, `onto`
— the commit to sit on when the record names one, `''` otherwise — unique
per change, deleted with it), so a newer record replaces the older with a
higher `seq`, then wakes the worker. It records nothing for a change
that is `tooDeep` (`depthOf` above the limit, counted rather than read
from `restack_reason`, which a restack conflict replaces). When the
workspace's `change.Workspace.AutoRestack` (the effective
`changes.auto_restack`, filled by `changeWorkspaces` in
`internal/hub/change.go`) is false, it records nothing either:
`flagBehind` re-reads the change and sets `needs_restack` with
`RestackAutoOff` (`auto_restack_off`) when it is `behind` the commit
`onto` or its link names, so every caller (`LinkVersion`'s dependants,
`linkTo`, `unflagDeep`, `OnLanded`, `RequestRestack`) gets the setting
through it. `RunRestacks(ctx)`
(`restack_queue.go`) is one goroutine: it runs a pass at the start, which
runs what was pending when the hub stopped, and one each time it is
woken; a pass reads, in a `Store.Tx` (so it waits for the transaction that
recorded it), the lowest `seq` not marked failed in memory, runs it, and
repeats until none is left. A restack that returns an error, or loses the
swap 5 times, is logged as `the restack failed` (`workspace`, `number`,
`error`; the hub's logger scrubs the error's text) and marked failed with
its `seq`, so it stays pending and runs again when a newer record
replaces it (a version of the change itself renews it) or at the next start (the map lives in the component, which
a start builds anew); the pass goes on with the next record. Every
failure of a restack is an error value; nothing in the worker ends the
process.

The restack (`restack.go`, `restack(ctx, job, caller, session)`) reads the
change in a `Store.Tx` (gone, or neither draft nor open: the record is
removed; for a job of the worker, `restackJob.background`, `skipsRestack`
skips a change that is `tooDeep` now, and one whose job has the trigger
`version` or `manual` and no `onto` that no longer has a draft or open
`depends_on` change nor a `restack_onto`: the record is removed and its
mergeability check scheduled) and the commit it must sit on (`sitOn`: the head of the
`depends_on` change while that one is draft or open, else the record's
`onto` or the change's `restack_onto`, else the head of the target branch
in the target's repository). Outside any transaction it resolves the
source branch in the source workspace's repository; when the head has the
commit and the commit has the change's `base_sha` (`sitsOn`, two
`IsAncestor`; with no base the first alone), the restack is up to date —
a dependant whose base is the head of an abandoned change below has that
head too, and is not: in a transaction,
the record is removed, a `needs_restack` with reason `conflict` or
`auto_restack_off` is cleared, `restack_onto` is cleared, and the
mergeability check is scheduled when a record was removed or a flag
cleared. Otherwise it replays `base_sha..head` onto the commit with
`(*gitrepo.Repo).RebaseFromResolving(ctx, base, head, onto, at,
resolutions)` (the seam `rebaseFunc`), with no resolutions
`RebaseFrom`, which differs from
`Rebase` only in replaying the commits after `base` instead of those
`onto` lacks, so that the old commits of a change below that was
force-pushed or landed are not replayed; its conflict report's `base_sha`
is `base`. A conflict ends the restack (`restackConflicted`, in
`conflict.go`): in one `Store.Tx` it reads the change and the commit to
sit on again (a new head or a moved commit: the attempt starts again),
removes the record, sets `needs_restack` with reason `conflict`
(`SetNeedsRestack`) and then stores `mergeable` `conflict` with the report
through `StoreMergeability` for the pair of the head it read and the
commit it replayed onto, so that the `parley.change.conflict` that
function writes on a transition carries `needs_restack` `true`. The report
(`restackReport`) is the replay's, with `target_sha` that commit,
`head_sha` the head, `base_sha` the change's base, and no change; the
landed changes and each file's `landed_by` are named by the attribution
(`Deps.Attribute`), before the transaction. No branch moves and nothing is recorded for the
changes above. Otherwise (`swapRestack`), in one `Store.Tx`,
it reads the change and the commit to sit on again (a new `head_sha` or
`base_sha`, or a moved commit: the attempt starts again; a replay that
returned the head it read is up to date, as above, with nothing moved),
moves the source branch with `CompareAndSwapRef` on the
head it read (`ErrRefMoved`, also for a ref a push has locked: the
attempt starts again, at most 5 in all), records the version with
`RecordVersion` (kind `restack`, by the zero `Caller` for the hub, the
session of the change's current version with `VersionInput.SessionChecked`
set, so the session linker is not asked again; without a session linker
the session is dropped), clears `needs_restack` unless its reason is
`stack_too_deep` and `restack_onto`, writes `parley.change.restack`
(`EventRestack`, metadata `version`, `head_sha`, `onto_sha`, `trigger`)
after the version's event, schedules the changes whose target is the
moved branch (`ScheduleTargetMergeable`, since no ref-updated stage saw
the move) and removes the record by its `seq`, so that a newer record
written meanwhile stays. When that transaction fails after the swap, the
branch is set back with a compare-and-swap. The version runs the stack
linker like any other, which records the restacks of the changes above
it, so a stack is restacked bottom first.

The manual restack, `POST …/changes/:number/restack` (`restackRoute`,
`restack_handler.go`, registered as a write route with `changes:write`
and the need `read`, on the contributor table's pattern), follows the
order of `modify` without using it, since its git work runs outside the
transaction and a conflict is answered after its transaction committed:
in one `Store.Tx`, `changeOf`, `AuthorizeModify`, the optional body
(`decodeOptional`; `resolutions` read by `readResolutions`,
`resolutions.go`, into a map by path of `gitrepo.Resolution`, any invalid
list `400 invalid_request`; `session_id` judged with the session linker's
rule, `400 unknown_session`), `workspaceUsable`, `409
change_closed` for a closed change, and the commit to sit on (`sitOn`,
with the `onto` of the change's pending record; `""` is `400
target_not_found`). It then runs `restack` with a job of trigger
`TriggerManual` (`manual`) that carries the `seq` of the change's pending
record, so that the record is removed when the restack ends, the
resolutions (`restackJob.resolutions`, which the worker's jobs leave
nil), and without
`background`, so that a change too deep in its stack is restacked too
(its flag stays) and the version guard is asked; the caller is the
version's creator and the events' actor, and the body's session (else the
replaced version's) is recorded with `SessionChecked`. Outcomes: restacked
and up to date answer `200` `{status, change}` with the change re-read;
a conflict `409 rebase_conflict` with `restackConflicted`'s report as
`details`; a lost race `409 ref_moved` with the branch's head read again
as `details.current_sha`; `gitrepo.ErrMergeCommit` `400
merge_commits_unsupported`; any other error `500 internal`. The interface
`VersionGuard` (`CheckVersion(ctx, caller, ch, base, head)`), registered
with `(*Component).RegisterVersionGuard` (a collaborator of the instance,
nil removes it), is asked in `restackOnce` once per attempt of a job that
is not `background`, after the replay and before the hook before the
swap; its error ends the restack unchanged, with nothing written. The hub
registers no version guard.

The stack route, `GET …/changes/:number/stack` (`stackRoute`,
`stack_handler.go`, a read route with `changes:read` and the need `read`,
on the contributor table's pattern), resolves its change with `changeOf`
and answers `(*Component).Stack(ctx, ch)`: a `Stack` (`target_branch` and
`[]StackSummary`, each the nine members of a summary with a
`StackLanding` holding only `status`). `Stack` walks `depends_on` down from
the change with `linkOf` (stopping where a change would repeat), then up
level by level through `dependants` (draft and open changes only), each
level sorted by number, which orders the changes above by depth from the
bottom and then by number; it reads each change with `readChange` from
the store. The binding of a token bound to a fork is read from the
context (`confinedTo` on a request carrying it), so that only the changes
whose source is the fork are listed, for the route and for the expander
alike. `composeChanges` registers `Stack` as the expander of `stack`.

`(*Component).OnLanded(ctx, workspace, number, landed)`
(`restack_queue.go`) is the entry point for a change that landed as the
commit `landed`: in a `Store.Tx` of its own, since the landing has ended,
each direct dependant gets `depends_on` nil (`SetDependsOn`, its base
kept, so that only its own commits are replayed), `restack_onto` the
landed commit, its depth recomputed, then `pendRestack` with
`TriggerLand` (`land`) and `onto` the landed commit; the changes further
up keep their links. The landing part registers an after-landing hook
that calls it for each landed change, with the commit the target held once
that change landed (see [Landing](#landing)). `stacks.RequestRestack(ctx, tx, ch,
trigger)`, the `Restacker` an abandon with `DependantsReparent` calls
after it has set the dependant's `depends_on`, recomputes the depth of
`ch` and the changes above it and calls `pendRestack(ctx, tx, ch,
trigger, "")` in the abandon's transaction; `sitOn` then resolves to the
head of the change `ch` now depends on, or the target head. The tests set the replay (given no resolutions) and a
hook before the swap through `export_test.go` (`SetRestackSeams`), hold
the worker (`HoldRestacks`) and run passes in the test's goroutine
(`RestackPass`). `ReadChange(ctx, q, workspace, number)` reads
one change, through the caller's transaction or, with `nil`, the store;
an unknown change is `change.ErrChangeNotFound` (`404 change_not_found`).
The change's `dependants` are not stored: every read derives them as the
ascending numbers of the draft and open changes whose `depends_on` names
it.

The lookups (`lookup.go`) read through a `Querier` the same way, `nil`
being the store, and answer "none" with a nil error:
`OpenChangeOfBranch(ctx, q, workspace, branch)` is the draft or open
change of a source branch (`change.BranchChange{WorkspaceSlug, Number,
Version, State}`, named under its target, the parent for a fork's
branch); `ChangesInto(ctx, q, workspace, branch)` the numbers of the
draft and open changes whose target is the branch, ascending, which
`ScheduleTargetMergeable` and the landing's follow-up use;
`DraftAndOpen(ctx, q, workspace)` the numbers of the draft and open changes
of the workspace whatever their target branch, ascending (a repository's
include those from its forks), which a replacement of the policy
re-evaluates; `ChangeOfCommit(ctx, q, workspace, sha)` is the change that
landed the commit (a `change_landed_commits` record) or else has it as a
version head, the lowest number first (`change.CommitChange{WorkspaceSlug,
Number, Version, Landed}`, `Version` the highest version with that head
or, for a landed commit, with the record's `version_sha` as head, nil for
none), a fork's slug asking for the parent's changes whose source is the
fork; `ChangesOfSession(ctx, q, session)` lists the `change.Key`s
(`workspace_slug`, `number`) of every change with a version of the
session, across workspaces, ordered by slug and number;
`ChangeNumbersOfSource(ctx, q, workspace)` lists every change, in any
state, whose source workspace it is (`change.SourceChange{WorkspaceSlug,
Number, Branch, State}`), ordered by number; `ReadVersions(ctx, q,
workspace, number)` reads every version of a change, oldest first, with
the credential that created each (`Version.Credential`, not marshalled);
`OpenCounts(ctx)` counts the draft and open changes per workspace slug.

The setters (`setters.go`) store, in the caller's transaction, what other
features compute, set `updated_at`, and answer the change-not-found error
for an unknown change: `SetDependsOn`, `SetBase` (the base of the current
version, with stats, touched paths and patch id measured again against it
in the change's own repository, on the change and on the version, and the
version's `patch_equivalent_to` set again to the latest earlier version
with the new patch id, `null` for none; a base of `""` is none, measured
from the empty tree),
`SetMergeable` (the conflict report is kept only with `conflict`),
`SetNeedsRestack` (the reason is dropped with `false`), `SetLanding`,
`SetLanded` (state `landed`, the `landed` object and the landed-commit
records), `SetReverts`, `SetAbandoned` (state `abandoned`
with a reason; for a fork source it tells the `ForkActivity` recorder,
whose error is logged) and, from `Open` and the edit, `SetAutoLand`
(`open.go`), which stores who turned `auto_land` on beside the flag (the
columns of the schema step `AutoLandSetterStep`, `change_auto_land_setter`:
the user's id, NULL for the admin token, the credential type and the
token's `paths` globs, NULL for none), forgets them when it is turned off
and changes nothing, `updated_at` included, for the value it already has;
`AutoLandSetter(ctx, q, workspace, number)` reads them back as an
`AutoLandSet`, not found while `auto_land` is off. The move to `landed`
tells no recorder. `reverted_by` is not stored by a setter: every read of
a change derives it, in the column list of the read (`changeColumns`), as
the highest number of the workspace's changes whose `reverts` is the
change's number and whose state is `draft`, `open` or `landed`, so that no
move of a reverting change, an abandon by a deleted source included, can
leave it stale.

What a deleted or lost source does to a change is in `retention.go`; its
functions run in the caller's transaction, and those a delete calls read
only the change record, so a delete may call them after the workspace's
own record is gone. `SourceLost(ctx, tx, slug)`, the clone observer the
changes part registers on the workspaces, moves every draft or open
change whose source is a branch the copy of `slug` does not have to
`abandoned` for `source_lost` through `Abandon`, by the system, and
leaves a change whose branch is there alone; it opens the copy only when
the workspace is the source of such a change. `SourceDeleted(ctx, tx, workspace, branch, actor)` moves the draft
or open change of the source branch (`workspace` being the source
workspace, as for `RecordVersion`) to `abandoned` for `source_deleted`
through `Abandon` (below), so a fork source's recorder is told, and a
branch with no such change changes nothing. `ForkDeleted(ctx, tx, fork, actor)`
moves every draft or open change whose source is the fork, all of them
the parent's, to `abandoned` for `source_workspace_deleted` without
telling the recorder, and leaves landed and abandoned changes alone; a
workspace's own changes are never its concern, so a repository's slug
changes nothing. Both write one `parley.change.abandon` per change (the
actor given, `resource_type` `change`, `resource_id` the number, the
target workspace, the change's session, and the metadata `reason`,
`ref`, `version` and `head_sha`), and keep every version and version
ref: a later move of the branch records no version, and a new open of
the same source takes the next number. `WorkspaceDeleted(ctx, tx,
workspace)` deletes the workspace's changes, whose versions, session
links and landed-commit records cascade, and its counter, so a later
workspace of the same slug numbers from 1; the changes of other
workspaces stay, also those whose source it was. The ref-updated stage
calls `SourceDeleted` and the teardown remover the other two (below).

The three write routes on one change (`PATCH …/changes/:number`, `POST
…/abandon`, `POST …/restore`; handlers in `edit.go` and `lifecycle.go`)
share the unexported `modify` (`authz.go`), which runs in one store
transaction, in order: `changeOf` (`404 change_not_found`, with the
confinement of a fork-bound token on the parent), the caller rule
`change.AuthorizeModify(r, ch)`, exported for the other routes that modify
a change (nil for the admin token, a caller whose access decision is
`owner` or `admin`, and the user who is the change's `author.actor_id`,
whatever their credential; else `403 forbidden`), the route's body
parser, which returns the route's operation (`400`), `workspaceUsable`,
the target workspace's `Unusable` (`409 workspace_busy`,
`workspace_not_ready`, `workspace_archived`), then the operation, and
answers the change re-read in the transaction. The routes declare
`changes:write` and the need `read`, so a member who opened a change
reaches it. The edit (`edit.go`) decodes the body into optional members
and checks each before the transaction's writes; its operation applies,
in order, the closed-change rules (`409 change_closed`), the intent rule
on the change as the edit would leave it (`intentRule`, `400
intent_required`), the
`AutoLandGuard` for a body with `auto_land`, then one `update` of the
members whose value differs and `SetAutoLand`; a `depends_on` member goes
to the `StackLinker`'s `SetDependsOn` for a draft or open change while
one is registered, and is ignored otherwise. A `state` other than the
change's own is completed last by `moveState`: `reevaluateLanding`, then,
for a draft marked ready, `parley.change.open` by the caller with
`was_draft` `true` (`emitOpened`, which `Open` shares) and
`AnnounceLandable`, or, for a change made a draft, `ForgetLandable` and no
event; neither schedules a mergeability check, since the head and the
target stay. A differing `target_branch` is checked before any write by
`checkRetarget`: the target rules go through `checkTarget` (`open.go`),
which `Open`'s `targetBranch` calls too, so a missing branch, the
integration branch of a carry-patch workspace and the change's own source
branch get the answers of an open, and a change with a `depends_on` or a
draft or open dependant is `409 conflict`. `retarget` then writes, after
the other members and before `moveState`, the target branch and the base,
patch id, stats and paths `measureMove` finds against it on the change
row only (`SetBase` would rewrite the current version too), runs
`reevaluateLanding`, `ForgetLandable` and `ScheduleMergeable`, and writes
`parley.change.version` by the caller with kind `retarget`
(`KindRetarget`, an event kind no stored version has) for the current
version. A `state` or a `target_branch` equal to the change's own is no
edit on a draft or open change, dropped before the intent rule, so an open
change left without an intent by a later `changes.require_intent` takes
`{"state":"open"}` as no edit; the closed-change rules count either like
any other member. The edit checks `title`, `intent` and `labels` with
`checkTitle`, `checkIntent` and `checkLabels` (`open.go`), which the open
uses too, so their answers are the open's.
Every abandon of a change, by the route, a deleted source branch and the
other callers in the hub, goes through
`(*change.Component).Abandon(ctx, tx, workspace, number, reason, actor)`,
which is `SetAbandoned`, `ForgetLandable` and `parley.change.abandon` with
the given reason; it touches no ref and leaves the change's dependants as
they are. It leaves the change's waiting mergeability check waiting,
since the caller's transaction may still be rolled back, which would
leave a draft or open change `unknown` with no check; a check that runs
stores nothing for a change that is no longer draft or open. The abandon
route drops the waiting checks of the changes it abandoned once its
transaction has committed, and `Reconcile` drops a change's after the
transaction that abandoned it; a deleted source branch (`SourceDeleted`)
and an open that finds its source gone (`followSource`) leave theirs to
the worker. The abandon route's operation (`lifecycle.go`)
decodes an optional body (`reason` a string, `dependants`
`DependantsReparent` or `DependantsAbandon`), refuses a closed change
(`409 change_closed`) and, without a `dependants` member, one with draft
or open dependants (`409 has_dependants`). `DependantsAbandon` collects
the whole stack above (the dependants, transitively) and abandons it
after the change, in the one transaction. `DependantsReparent` abandons
the change, sets each direct dependant's `depends_on` to the change's own
with `SetDependsOn`, then hands each to the `Restacker`
(`RequestRestack(ctx, tx, ch, trigger)`, declared in `lifecycle.go` and
registered with `(*change.Component).RegisterRestacker`, which the stack
feature implements) with the trigger `abandon` (`TriggerAbandon`) inside
the transaction, whose error rolls the abandon back (the hub registers
`stacks`, above); with no restacker registered it calls
`ScheduleMergeable` on each instead.

The restore route's operation (`restore` in `lifecycle.go`) checks, in
this order and before it writes anything: the change is abandoned (`409
change_not_abandoned`); its source workspace has a record, read through
the `WorkspaceReader` (`409 source_workspace_gone`); no draft or open
change has its source (`activeOfSource`, answered by `changeExists`,
`409 change_exists` naming that change's ref, the refusal an open gives);
in the source workspace's repository either the source branch exists or
the last version's head is still an object (`ResolveBranch`, then
`Missing`, which sees a fork's parent's objects through its alternates;
else `409 conflict`); the intent is not empty while the target
workspace's `RequireIntent` is set (`intentRule`, shared with the edit,
whose refusal `intentRequired` is the open's, `400 intent_required`). A missing branch is then created at the
last version's head with `CompareAndSwapRef(ctx, ref, head, "")`;
`ErrRefMoved` means it appeared in the meantime, and its head is read
and taken as for a branch that exists. A branch the restore created is
deleted again (a compare-and-swap to the zero id) when the operation
fails after it. In the route's transaction it then sets the change `open`
with no `abandon_reason`, a violation of the unique index on the active
source being answered as `409 change_exists`, runs `reevaluateLanding`,
calls `ScheduleMergeable` and, for a branch it created,
`ScheduleTargetMergeable` on the source workspace and branch (the
create-only ref update bypasses the ref writer, whose `TargetsUpdated`
stage would schedule the changes into the branch), writes
`parley.change.restore` (`EventRestore`,
metadata `number`, `ref`, `version`, `head_sha` of the last version),
tells the `ForkActivity` of a fork source (its error logged), records a
branch head other than the last version's through `RecordVersion` with
kind `ref` and the caller, now that the change is open, and calls
`AnnounceLandable`.

The mergeability worker (`mergeable.go`) keeps, on the component, the set
of changes whose check is due or waits (one entry per change, whatever the
number of triggers), the running ones, and when each check of a change
last started. `RunMergeability(ctx)` runs four goroutines that take the
first waiting change with no running check whose last start is at least
`Deps.MergeableDebounce` ago, and otherwise wait for a new entry, an
ended check or the next due time. A check reads the change and its
onto-commit in a `Store.Tx` of its own, which waits for the transaction
that scheduled it: the head of the `depends_on` change while that one is
draft or open, else the head of the target branch in the target
workspace's repository. A change that is neither draft nor open, or that
has a pending restack (`change_restacks`), is dropped, since its head
still holds the old commits of the change below: the end of the restack
schedules it again, or stores its conflict; a missing target branch leaves `mergeable` `unknown` and runs
`reevaluateLanding`. It reads the cause with `LandingCause` (`draft` for
a draft, which is not measured, else the structural reason `moveCause`
decides from the target's head, the commits ahead, the merge base and
their parents, without the diff, the stats and the patch id `measureMove`
adds, `target_not_found` here). The registered landing evaluator answers
with that cause and the current version; without one, the cause blocks
the landing, else it is ready. Otherwise the check runs
`(*gitrepo.Repo).TrialRebase(ctx, onto, head)` under a 30 s deadline,
or, for a change in a stack (`ownBase`: it has a `depends_on`, a
`needs_restack` or a `restack_onto`) with a `base_sha`,
`(*gitrepo.Repo).TrialRebaseFrom(ctx, base, head, onto)`, which replays
only the change's own commits, `base_sha..head`, as its restack does, so
that a check's report and a restack's describe the same commits and the
old commits of a change below are never reported; either writes no ref,
index or work tree; an error or the deadline leaves
`mergeable` `unknown` and logs `the mergeability check failed` with
`workspace` and `number`, and nothing retries it before the change's next
trigger. The result is stored in a second `Store.Tx` only while the change
is draft or open and its head, onto-commit and base are still those computed,
else it is discarded and the change scheduled again. A change that holds
the conflict a restack stored after the check was scheduled
(`holdsRestackConflict`: `mergeable` `conflict`, `needs_restack` with
reason `conflict`) keeps it: the result is discarded, since the
restack's report already describes the pair. A check scheduled in
a transaction that is rolled back computes the change as it is.

`(*change.Component).StoreMergeability(ctx, tx, workspace, number, head,
onto, report)` (`mergeable.go`) is the one function that stores a computed
result, in the caller's transaction (the worker's, and the restack's of
package 24): it reads `mergeable_last`, the value the last check computed,
calls `SetMergeable` (`clean`, or `conflict` with the JSON of the
`gitrepo.ConflictReport`), stores the pair and the value in
`mergeable_head`, `mergeable_onto` and `mergeable_last`, writes
`parley.change.conflict` (`EventConflict`, by the hub, metadata `version`,
`head_sha`, `files`, `needs_restack`) only for a conflict whose last
computed value was not `conflict`, re-evaluates a landing still blocked for
`target_not_found`, and runs `AnnounceLandable` last.
`(*change.Component).AnnounceLandable(ctx, tx, workspace, number)`
(`landable.go`) is the one writer of `parley.change.ready` (`EventReady`,
by the hub, metadata `version`, `head_sha`): it does nothing while
`mergeable` is `unknown`; an `open` change with `landing.status` `ready`
and `mergeable` `clean` whose `ready_announced_head` is not its head gets
the event and the head stored; any other change has the column cleared
through `ForgetLandable(ctx, tx, workspace, number)`, which the abandon,
the move to draft (and the retarget) call too. The landing policy's
re-evaluation (`ReevaluateIn`) calls `AnnounceLandable` after `SetLanding`,
in the transaction that stores the landing. `emit` (`open.go`) adds
`number` and `ref` to the metadata of every change event. An error of
the event writer rolls the storing transaction back, which leaves
`mergeable` `unknown` and is logged as a failed check.

Triggers set `mergeable` `unknown` (`conflict` null) in the triggering
transaction and schedule the check at once:
`ScheduleMergeable(ctx, tx, workspace, number)` for one draft or open
change (restore; the retarget and the restack call it too), and
`ScheduleTargetMergeable(ctx, tx, workspace, branch)` for the draft and
open changes of the workspace whose target is the branch (the target
stage, the sync observer and a restore that creates its source branch).

`(*change.Component).Reconcile(ctx)` (`reconcile.go`) is the start-up
reconciliation, which repairs what a stop between a ref update and its
store transaction lost. It lists the draft and open changes and reconciles
each in a `Store.Tx` of its own: it reads the source branch in the source
workspace's repository; at the head of the current version it writes
nothing, `updated_at` included; at another commit it calls `RecordVersion`
with kind `ref` and `change.Caller{}` (actor `system`, no credential), so
the version, its ref, `parley.change.version` and the check come as for
any new version; a missing branch of a source workspace that has its record
calls `Abandon` with `source_deleted` by the system, which keeps the
versions and their refs. A change that cannot be reconciled, because a
repository cannot be read, the version cannot be recorded, or its source
workspace has no record (a fork whose delete did not settle its changes),
is left as it is and logged at error level as `the change was not
reconciled with its source branch` with `workspace` and `number`; the
others are reconciled all the same and the start goes on. Only a failure
to list the changes fails the start. Then every draft and open change whose
`mergeable` is `unknown` is added to the worker's waiting set without a
write; the worker keeps those entries and runs them once it starts after
the bind.

The hub part connects the component to the features that move a change's
source or ask about changes, all at composition, on the instances that
call them:

- `c.RefWriter().RegisterRefUpdatedStage((*change.Component).RefUpdated)`
  (`refstage.go`). The ref writer runs the stage in the store transaction
  of every accepted write of refs, after the refs are locked and before
  they move. Unless `Via` is `land`, for each allowed update of
  `refs/heads/<b>`, a `Direct` one included: a `Deleted` one calls
  `SourceDeleted` with the caller; with `VersionKind` `push`, `commit`,
  `ref` or `rebase` the others call `RecordVersion` with that kind, the
  caller (`pushrule.Caller`'s `UserID`, `CredentialType` and
  `CredentialID`), the paths and the session, and report
  `pushrule.Version{WorkspaceSlug, Number, Version}` at the update's index;
  `VersionKind` none records nothing. `StageInput.Workspace` is the
  workspace written to, the source; the change lives in its target. An
  error, such as `400 unknown_session`, stops the write: no ref moves,
  nothing of the transaction is kept, and the source control API answers
  the API error as it is. A request that moves the source and deletes a
  packed branch records its version while git holds the `packed-refs`
  lock, since the version ref is a new loose ref.
- `c.SCM().RegisterChangeLinker(changeLinker)`: `FindOpen` is
  `OpenChangeOfBranch`; `CheckOpening` is
  `(*change.Component).CheckOpen(ctx, q, in)` (`service.go`), which runs
  `Open`'s whole check order, `Authorize` included, and writes nothing,
  with `Head` the commit the write moves the branch to; `Open` is `Open`
  in a store transaction of the linker, since the writes call both
  outside any. Both take version kind `commit`, the write's session as
  `TriggerSession`, and "" of a title, intent or target branch as the
  default, and no `CheckPaths`.
- `c.AdjustSCM` sets `scm.Deps.Changes` to `changeLookup`:
  `OpenChangeOfBranch` and `ChangeOfCommit` with the numbers as `int`
  (`scm.CommitChange.Version` is `nil` when the change has no such
  version).
- `c.Workspaces().RegisterBranchMoveObserver(syncVersions)`: a branch a
  sync moved records a version of kind `sync` with the system actor
  (`change.Caller{}`), in a `Store.Tx` of its own.
- `c.RefWriter().RegisterRefUpdatedStage((*change.Component).TargetsUpdated)`,
  after `RefUpdated`: for each allowed update of `refs/heads/<b>` of the
  workspace written to, whatever its `Via` (`git`, `api`, `land`) and
  whether it moves, creates or deletes the branch,
  `ScheduleTargetMergeable` in the write's transaction; it only reads and
  writes the store and records no version.
- `AddVersionObserver(MergeableObserver())`: every new version, whatever
  recorded it, schedules the check of its change and of the draft and
  open changes that depend on it, in the version's transaction.
- `c.Workspaces().RegisterBranchMoveObserver(syncTargets)`, after
  `syncVersions`: `ScheduleTargetMergeable` for the branch a sync moved, in
  a `Store.Tx` of its own.
- `c.Workspaces().Teardown().RegisterRemover`: in the delete's
  transaction, `ForkDeleted` with the system actor and then
  `WorkspaceDeleted`, for the deleted workspace and for each of its forks,
  and for a fork the fork feature removes.
- `c.Forks().RegisterChangeReader(forkChanges)`: `ChangesFrom` is
  `ChangeNumbersOfSource` (branch and state).
- `RegisterForkActivity(forkActivity)`: `(*fork.Component).RecordActivityIn`
  in the caller's transaction.
- `c.Events().SetChangeSources`: the change's source workspace through
  `ReadChange`, "" for a change that does not exist.
- the git server's fork view (`forkView`) names the numbers of
  `ChangeNumbersOfSource` as `gitserver.ForkRefs.Changes`.
- `c.Receiver().RegisterMappingStage`: a `gitserver.ChangeMapper` over
  `pushChanges`, created with no logger and bound to the hub's logger
  (`ChangeMapper.Bind`) in the start hook. `pushChanges` is the mapper's
  `gitserver.ChangeService`: `OpenChangeOfBranch` is the component's;
  `OpenChange` is `Open` in a `Store.Tx` of its own, since the mapping
  runs after the push's ref transaction, with version kind `push`,
  `AfterMove`, no `CheckPaths`, the push's session as `TriggerSession`,
  and the request's target branch, title, intent and draft flag (nil
  being the defaults), and answers a `409 change_exists` with
  `*gitserver.ChangeExistsError` naming the change `OpenChangeOfBranch`
  finds, or, when that lookup fails or finds none, with an error that does
  not wrap the refusal, so that the mapper reports `internal error` rather
  than words the push's refusal; `RecordPushVersion` is `RecordVersion` of kind `push` and then
  `OpenChangeOfBranch`, in one `Store.Tx`, as the ref-updated stage
  reports a version. The caller is `change.CallerOf` of a PAT's principal,
  and otherwise the push caller's user, credential type and id.

The route `POST /workspaces/:slug/changes` (`routes.go`; scope
`changes:write`, read access to `:slug`, an idempotent create, and on the
parent of a token's bound fork a contributor route) decodes the body, a
member of the wrong type being `400 invalid_request`, opens its own
transaction and calls `Open` with version kind `ref` and `CheckPaths`.
Two opens of one source cannot interleave, since the store serialises
write transactions: the second finds the first change and answers `409
change_exists`, naming its ref in the message and the details, and the
unique index answers the same should one slip through.

The reads (`read.go`) are four `GET` routes with scope `changes:read` and
read access to `:slug`, registered with the patterns of the contributor
table of `internal/access` so that a token bound to a fork reaches them on
its parent: `/workspaces/:slug/changes/:number`, `…/:number/versions`,
`…/:number/versions/:version` and `/workspaces/:slug/changes/by-commit/:sha`,
which the router prefers to `:number` as the more specific pattern. They
read the store outside any transaction. The three `:number` routes resolve
their change in one place, the unexported `changeOf`: a number that is not
a positive integer, or that `readChange` does not find under `:slug` (so
any number on a fork's slug, since a fork holds no changes), is `404
change_not_found`, and so is, for a token bound to a fork calling on the
fork's parent (`access.BindingOf`, relation `parent_of_bound_fork`), a
change whose source workspace is not the bound fork. The version list is
paged by version number (`readVersionsAfter`) in the list envelope, and one
version (by number or `latest`) carries its `ref`. By-commit asks
`ChangeOfCommit` of `:slug`, or of the bound fork for such a token, and
answers only a change of `:slug`, so a fork's slug finds nothing though
`ChangeOfCommit` answers it from the parent.

The list (`list.go`), `GET /workspaces/:slug/changes` with the same scope,
need and contributor-table pattern, reads the store outside any
transaction. It lists the changes of `:slug`, or, when the access decision's
record is a fork, those of its parent whose source workspace is the fork,
and, for a token bound to a fork calling on its parent (`confinedTo`), only
those whose source is the bound fork. The filters are SQL conditions
combined with `AND` (`label` through `json_each` over the stored labels,
`landing_status` through `json_extract`, `session_id` through
`change_sessions`), except `path`, whose glob is matched with
`identity.MatchPath` over the stored paths while the rows are read in
order, before the page's limit is counted. Paging is by keyset in a total
order: `updated_at` in whole seconds descending, then `number` descending
(position `{s, n}`), or `number` ascending (`{n}`), so the order is that of
the times the caller sees. `stack_of` reads every change's `depends_on`
link of the workspace, walks down to the bottom and up through the
dependants, keeps the stack members that meet the filters other than
`state`, and pages by index in the stack (`{i}`). Items are `listItem`s,
the change object without `conflict`, and without `paths` and
`paths_truncated` unless `include=paths`.

`include` on the change (`expand.go`) takes `versions`, which the
component serves from `readVersions`, and `approvals`, `checks`, `stack`
and `overlaps`, each served by the `change.Expander` (`func(ctx, Change)
(any, error)`) registered on the instance with
`(*change.Component).RegisterExpander(name, fn)` (nil removes; a name
outside those four panics); a name without an expander adds no member, and
the members follow the change's own in that order. The hub registers
`(*Component).Stack` for `stack`, and the policy component's
`ExpandApprovals` and `ExpandChecks` for `approvals` and `checks`, and
the awareness component's `Overlaps` for `overlaps` (see
[Awareness](#awareness)).

The diff (`diff.go`), `GET /workspaces/:slug/changes/:number/diff` with
the same scope, need and contributor-table pattern, resolves its change
with `changeOf` and its versions (`version`, default the current one, and
`against`) from the store, then opens the target workspace's repository,
where every version's head is held under its version ref, a fork
source's too. A base or head commit of a named version missing from it
(`(*gitrepo.Repo).Missing`), as after a reclone, is `404 ref_not_found`.
Both forms reuse the source control API's diff computation,
`scm.ComputeDiff(ctx, repo, from, to, scm.DiffQuery{Globs, Keep, Patches,
AsPatch, Context})`, which `GET …/scm/diff` also calls, and its limits
(`scm.MaxDiffFiles`, `scm.MaxPatchBytes`), and answer the patch form
through `scm.WritePatchText`. The plain diff is the version's
`base_sha..head_sha`. The interdiff reads each version's own file changes
and patches (`DiffChanges`, `DiffPatches` against its base) and compares
the patches of each path after dropping the `index` line and the hunk
headers' positions (a binary file, or a patch over `MaxPatchBytes`, by its
blob ids); a path either version touches is listed, changed when the two
differ or only one touches it. The patch id stored for each version is a
hash of the same keys, so patch equivalence and the interdiff agree. The
changed files come from `ComputeDiff` of the two heads with `Keep`
(which is given the whole `gitrepo.FileChange`) accepting an entry whose
path is a listed changed path, or a rename whose old path is one; a
rename's entry is listed at its new path when that is a changed path,
else at its old path, which then has no entry of its own. A changed path
the heads' diff does not name (the same content at both heads) is
answered `modified` with an empty patch and counted in `stats`, unless
that diff cut its list of files, and an unchanged one with the file change
of its own diff and no patch.

### Landing

`internal/hub/land.go` adds the part `composeLand`, after `composeChanges`
in `parts`. It creates a zero `land.Component` (`Land()`), adds
`land.SchemaStep` (the step `land_attempts`) and `land.JournalStep` (the
step `land_journal`), registers its `Routes()`
with `AddRoute`, registers `WorkspaceDeleted` as a remover of the
workspace teardown, which deletes a deleted workspace's attempts in the
delete's transaction, and binds the component in the start hook `land`
to the store (`Store`), the hub's git opening
`(*workspace.Service).RepoPath(slug)` (`Repos`), the change component
(`Changes`), `landWorkspaces` (`Workspaces`), an adapter over
`workspace.BySlug`, read through the querier it is given, with
`workspace.RequireReady` as `Ready` and the effective `land.strategy`,
`land.by_author` and `land.delete_source`, the access service
(`Access`), the event component (`Events`), the ref writer
(`RefWriter`), the hub's clock and logger, and the `[land]` options of
`config.Land` (`MaxAttempts`, `Workers`, `Timeout`, and the committer name
and e-mail as `Committer`);
`AdjustLand` adjusters run before the bind, and `Bind` defaults
`MaxAttempts` to 10, `Workers` to 8, `Timeout` to 5 minutes and `After`,
the clock a land request waits on, to `time.After`, and `Mergeability`,
the scheduler of the mergeability checks a land leaves, to `Changes`. No
session linker is registered. The part registers two after-landing hooks,
outside the start hook: one calls `(*change.Component).OnLanded(ctx,
workspace, number, landed)` for each landed change, the other
`(*fork.Component).RecordActivity(ctx, slug)` for each landed change whose
source workspace is not the target; each opens its own store transaction
and the hooks are called outside any. No gate, override rule, request
guard, target guard or final observer is registered here: the policy gate
is `composePolicyEvaluation`'s, and the override rule, the request guards
and the final observer are `composeLandRules`' (see
[Land rules](#land-rules)); the one target guard and a third
after-landing hook are `composeCarryPatch`'s (see [Carry-patch
workspaces](#carry-patch-workspaces)). The part binds
`Deps.Rules` to the push rule engine, which the revert route asks about
the branch it creates. The start
hook `land-journal`, after `land`, runs `(*land.Component).Reconcile`;
the start hook `land-recover`, after it, runs `(*land.Component).Recover`.
The part adds the background work `land_pool` (grace 5 s), which runs
`RunPool` until the stop. A test starts the hub with the pool paused by
setting `Deps.Paused` with `AdjustLand`; an adjuster that reads a flag of
the test lets a later start run unpaused.

The schema keeps `land_attempts` (one row per attempt, keyed by its
UUID, with the workspace, the target branch, the status, constrained to
the five, the trigger, the strategy, the result members, `commits`,
`conflict`, `blocked` and `override` as JSON, the override the request
carried (`override_requested`, JSON, which the attempt object does not
show and its run after a restart puts to the rule again), who requested
it with the credential's id, and `created_at` and `finished_at` in nanoseconds), with
indexes on the workspace with the status and with `created_at`, and
`land_attempt_changes` (keyed by attempt and position, the changes an
attempt covers bottom first, each with the version it had when the
attempt was created, that version's head and base, and the change's
source workspace and branch), deleted with its attempt, with an index on
the number. An attempt is inserted running; finishing it writes its final
status and its result members in one `UPDATE` that only a running
attempt of that workspace takes, so a second final status is refused and
a final attempt never changes.

The routes read the store directly. `GET …/changes/:number/lands` reads
the change through `(*change.Component).ReadChange` first, so a number
the workspace does not have is `404 change_not_found`, and selects the
attempts with a covered change of that number; `GET …/lands` adds a
condition per filter. Both order by `created_at / 1000000000` (the shown
second) and `id`, descending, and page by keyset on those two values.
For a token bound to a fork, calling on the fork's parent
(`access.BindingOf`, relation `parent_of_bound_fork`), `authz.go`'s
`confine` answers `403 token_bound` for a change whose source is not the
fork and `confineAttempt` for an attempt none of whose covered changes
comes from it; `GET …/lands` is refused by the binding guard, since it is
not a contributor route.

`GET …/changes/:number/conflict` (`conflict_handlers.go`) resolves the
change through `(*change.Component).RequestedChange`, the lookup of every
change route, so a change of another source answers a token bound to a
fork `404 change_not_found` rather than `403 token_bound`. Its one
function of the current result, `currentConflict` (`current_conflict.go`),
answers a landed or abandoned change from its stored report; for a draft
or open change it takes the pair of the mergeability check from
`(*change.Component).ReplayPair` (the onto commit, the change below's head
or the target head, and the base of a change in a stack) and answers the
stored report only when it is a conflict of that onto commit and the
change's head, otherwise it runs `change.TrialRebase`, the check's trial,
and attributes the result with `Attribute`. It writes nothing; a missing
target branch is `409 conflict`.

The land route (`routes.go`, `landRoute`) reads the change, then the
body and the `wait` query (`parseLand`), collects the changes it covers
(`collect`: the change, or for `stack` the changes down its `depends_on`
up to a landed one, bottom first, a cycle ending the walk), confines a
fork-bound token to them, admits the caller (`authorize`: the admin
token, a user for whom `Access.CanWrite` holds, or, while `land.by_author`
is on, the author of every change it covers, so that the author of a
change cannot land with `stack` a change below by someone else; else
`403 forbidden`), checks the
workspace's `Ready`, then the session (`session`: the registered
`SessionLinker`'s `Check`, `400 unknown_session` for a refused id and for
every id while none is registered), then asks the request guards
(`askRequestGuards`) with the caller, the workspace, the changes it covers
(`covered`) and the requested override, the first refusal being the
answer as the guard returns it. `create` then reads the changes again
in one store transaction, checks the preconditions there (`admit`: every
covered change open, `expected_version`, no running attempt covering a
covered change, found by `runningCovering`, and without `stack` every
change down `depends_on` landed; the first that fails is the `409`),
refuses with `409 version_mismatch` a land whose changes, each at its
version, are no longer those the route judged before the transaction
(`StartInput.judged`, compared by `sameVersions`; who may land, the
confinement and the request guards judged those), inserts the running attempt with each change's current version, head,
base and source, and has the linker `Record` the session's link in that
transaction; after it commits the attempt is handed to the pool
(`submit`). Since the store runs its transactions one at a time, the
check for a running attempt and the insert cannot be split by another
land of the same change. `admit` takes the id of an attempt to leave out
of that check, for the run of an attempt again after a restart. Nothing
is held across an attempt.
`answer` waits for `ended(id)`, `Deps.After(wait)` or the request's end,
whichever comes first, then reads the attempt and answers `202` while it
runs, `200` when it landed, and otherwise the `*server.Error` of `final`
with the attempt as its details: `409 land_conflict`, `409
policy_blocked` or `503 land_failed`. `(*land.Component).Start(ctx, StartInput)`
does the same from the workspace's `Ready` on, without a request and
without asking the request guards, for a land with the trigger
`auto_land`; nothing judged its changes before, so it compares none.

A land replays a change with `(*gitrepo.Repo).RebaseFromAs`, which takes
the landing identity as committer and says what each commit of the change
became. A replay that conflicts is reported in the hub's one
`gitrepo.ConflictReport`, its files, kinds and sides as the replay
classified them. `conflict` (`conflict.go`) completes the land's report:
`target_sha` the head the attempt replayed onto, `head_sha` and
`base_sha` of the stored version, `change` the number in a stack land.
`(*land.Component).Attribute(ctx, workspace, report)`, which the hub
hands the other producers of a report (comparison, server-side rebase,
mergeability check, restack) as a function value, adds the landed changes: when `base_sha` is
an ancestor of `target_sha`, it walks `CommitsBetween(base, target)`,
keeps the commits whose `ChangedPaths` meet a path of a conflicted file's
sides, and asks the landed-change lookup (`land.LandedLookup`,
`landed_lookup.go`) for each one's landed change, a `CollidedChange`
`{number, title, landed_sha, session_id}`; a commit it finds no change
for (a direct push) names none. `Deps.Landed` sets the lookup; nil is the
one over the change component, `(*change.Component).ChangeOfCommit` (a
landed commit only) then `ReadChange` for the title, session and
`landed.sha`. The changes found give `landed_changes`, once each and by
number, and each file's `landed_by`, ascending; a file no landed change
touched reads `landed_by` `[]`, and a history or lookup it cannot read is
its error, with both left empty, which every producer logs, keeping the
report.

The engine (`engine.go`) runs one running attempt: it reads the target
workspace and opens its repository, asks the policy gate once per covered
change, bottom first (`judge`), about the version the attempt covers (its
number, head and base, beside the change as it reads now), then tries up to `Deps.MaxAttempts`
times: it counts the try in `attempts`, reads the target head (a target
branch that does not exist ends the attempt `failed` `internal`; a land
never creates it), builds the commits
to land (`replay.go`: `build` replays each change's `base..head` of the
version the attempt stored with `RebaseFromAs` onto the head, the
landing identity `Deps.Committer`, default `Parley <parley@localhost>`,
as committer, and keeps one `Commit` per commit that did not become
`""`; a version without a base, whose history the target does not share,
is an error, since replaying from no base would land its whole history; a stack's changes bottom first, each onto the head the one below
ended at, so that one move lands them all; under the strategy `squash`,
`squash` (`squash.go`) makes of each change one commit with
`(*gitrepo.Repo).CreateCommit` on the head it was replayed onto, the
replay's tree, the author of the version's head commit with the zone of
its author date (`(*gitrepo.Repo).AuthorZone`, since `ReadCommits` reads
it in UTC) and the message `squashMessage` writes, and none when the tree is unchanged), and hands
the result to `commit` (`commit.go`). A `commit` that
finds the target moved since it was read starts the next try on the new
head; when the last try lost, the attempt ends `failed` `contention`. A
gate, store or git error other than a conflict ends it `failed`
`internal` and is returned to the engine's caller. On each try whose
build did not conflict and made a head the guards have not allowed yet
(the first, and each retry on a moved target), `judgeTarget` asks each
target guard (`TargetGuard.Check` with the workspace, the branch, the head
read and the built head) before the swap: a refusal ends the attempt `blocked` with `blocked` the top
change, status `blocked`, no requirements and the guard's reason for the
event, an origin rejection `failed` `origin_rejected` (its message
logged), and an error or a panic `failed` `internal`. Every final status
is written by `finish`, in the transaction that ends the attempt; `end`
runs it in a transaction of its own, `fail` ends the attempt `failed`
`internal`.

`hooks.go` holds four more registration points, each a list called in
the order of registration: `AddRequestGuard` (`RequestGuard.Check(ctx,
RequestInput{Caller, Workspace, Changes, Override}) error`, a non-nil
error refusing the request as given), `AddTargetGuard`
(`TargetGuard.Check(ctx, TargetInput{Workspace, Branch, Old, New})
(TargetAnswer{Refuse, OriginRejected}, error)`), `AddAfterLanding`
(`AfterLandingHook(ctx, AfterLanding{Attempt, Workspace, Update{Ref,
Before, After}, SessionID, Changes []LandedChange{Number,
SourceWorkspace, SourceBranch, LandedSHA}}) error`) and
`AddFinalObserver` (`FinalObserver(ctx, Final{Attempt, Result, Attempts,
Duration})`). Once a transaction that makes an attempt final has
committed, `concluded` (`after.go`) runs: for a landed attempt `follow`,
then `report`, which tells every final observer of it, `Duration` from
`created_at` to `finished_at`. `end` calls it for every attempt it ends,
the pool's timeouts included, `commit` after the record of a land, a
`noop` one included, and `Reconcile` after a land it records; a second
final status `finish` refuses tells nobody, so each attempt is reported
once. `follow` runs each step in `step`, which logs its error or its
panic and goes on: `deleteSource` per covered change (when the target's
`land.delete_source` is on and the branch is not the source workspace's
default branch, one store transaction in which `OpenChangeOfBranch`
finds no draft or open change of the source branch, a `PrepareRefs`
delete of `refs/heads/<branch>` expects the version's head,
`ScheduleTargetMergeable` of the source workspace and branch is written,
and the ref transaction commits before the store's; a branch that moved
is `ErrRefMoved` and kept. Since a change opens in a store transaction
that reads its source branch there, and the store runs its transactions
one at a time, a change opened from the branch meanwhile is either seen
by the check or finds no branch), `scheduleOthers` (`Deps.Mergeability.ScheduleMergeable`
for every change `(*change.Component).ChangesInto` gives for the target
other than those landed, in one transaction), then each after-landing
hook, with each change's landed commit its last `commits` entry, else
that of the change below it, else `onto_sha`. Nothing of the land
contacts origin or a forge.

`gate.go` holds the two registration points on the component: the policy
gate (`RegisterGate`; `Gate.Evaluate` takes the workspace, the change,
the version and the caller and answers a status, a reason, requirements
and a revision), whose default answers `ready` with revision 0 and in
whose place the landing policy registers its gate (see
[Landing policy](#landing-policy)), and the override rule
(`RegisterOverrideRule`), whose default passes nothing.
`judge` asks the gate before the first try only; an answer that is not
`ready` on an attempt with a requested override is put to the rule, and
one it lets pass counts as `ready` and sets the attempt's `override` to
the requested one. The first change whose answer is neither ends the
attempt `blocked` with the gate's reason kept in memory for the event. A
gate or rule that fails, or panics (`recovered`), ends the attempt
`failed` `internal`; the hub goes on.

`finish`, for an attempt that did not land, calls `stopped` in the same
transaction: one `parley.change.land_fail` (`emit`, which `settle` uses
too, adds `number` and `ref` to the metadata) on the change the conflict
report or the block names, else the top change, with `reason` and, for a
conflict, `files` through `event.PathList`; a conflict is also stored as
the change's mergeability through `(*change.Component).StoreMergeability`
with the version's head and the report's `target_sha`. A timeout ended
by the pool goes through `end` and `finish` like every other end.

`commit` is the atomic step. A build with nothing left (`noop`) is
recorded without moving anything. Otherwise the target moves through the
ref writer: `pushrule.AllowedVerdict` for the one update `refs/heads/<target>`
from the head read to the new head, without `Evaluate`, so a protected
target moves like any other. Before the move, a `land_journal` row is
committed in a transaction of its own, which first reads every covered
change again (`stillOpen`): one no longer `open`, abandoned or set back to
`draft` while the attempt ran, ends the attempt `failed` `internal`
without moving the target (a `noop` land checks the same in the
transaction that records it). A change abandoned after that transaction
and before the record is landed all the same, since the target holds it
by then. The row holds the attempt's id (one row per
attempt, deleted with it), its workspace, the ref, its old and new heads,
and as JSON the tries, the `commits` entries, the policy revision, the
override and the finish time, which the running attempt's row does not
hold. Then `Apply` with `Via` `land` and no version kind moves the ref; a
`ref_moved` outcome, which a deleted or locked target also gives, deletes
the row and returns to the engine for the next try. Once the ref moved
the engine runs on `context.WithoutCancel`: `settle` records the land in
one store transaction (`SetLanded` per change with `landed` and its
commit mapping, the attempt finished `landed`, one `parley.change.land`
per change), and `Finish` then writes the `parley.git.push` event. A
`settle` transaction that fails is tried again, up to three times in all
(only a running attempt is settled, so nothing is recorded twice). Every
`finish` deletes the attempt's journal row in its transaction. An error
after the move leaves the attempt running, its row kept for `Reconcile`.

The pool (`pool.go`) runs the attempts handed to it (`submit(a, from)`,
which never waits or fails), each in a goroutine of its own. A goroutine
takes one of `Deps.Workers` slots of a semaphore, in no promised order,
and runs the engine under a context with the deadline `from +
Deps.Timeout` (`from` is the attempt's creation) whose cause is
`errTimedOut`; the slot is given back when the engine returns, before
what follows the attempt's end: `concluded`, called in a pool run, is
deferred (`later`, through the run's context) and runs in the job's
goroutine once the slot is free, so that a slow after-landing hook or
observer holds no worker, and before `ended(id)` closes; the stop of the
pool waits for it like any run. When the
deadline passes first, waiting for a slot or running, the pool cancels
the context, which kills the attempt's `git` processes with their
process group, and ends the attempt `failed` `timeout` itself (`expire`),
unless the engine has entered the compare-and-swap: `commit` asks
`enterSwap` immediately before `Apply`, which refuses an attempt whose
context ended (its journal row is removed) and otherwise marks it, so that
from then on the engine ends it, `landed` when the target moved; a lost
swap unmarks it (`leaveSwap`). `end` turns any outcome of an attempt whose
context ended by its deadline into `failed` `timeout` and takes the second
final status `finish` refuses as done; a context ended otherwise, by the
stop of the hub, ends nothing: the attempt stays `running`. `ended(id)`
is a channel closed once the attempt is final or the pool let go of it,
for a waiter that then reads the store. Attempts submitted before
`RunPool` runs start when it does; once its context ended nothing starts,
and `RunPool` returns when every engine has returned. `Deps.Paused` holds
every attempt submitted without running or ending it.

`Reconcile`, before the listener binds, reads every journal row: one
whose ref holds its new head, or a commit after it (`IsAncestor`: other
writes may have moved the target on after a swap whose record failed), is
completed with `settle` in one transaction, as the engine would have (an attempt already final has its
row deleted, nothing written twice), and the ref writer's own journal
(`push-journal`) writes the `parley.git.push` of a move whose `Finish`
did not run; any other row moved nothing and is deleted, its attempt left
`running`. A row whose repository cannot be read or that cannot be
settled is logged and kept for the next start, its attempt `running` and
not run again by `Recover`. A second run finds no row. After a stop at any point of the step the hub thus comes up with
the target at its old head and the change `open`, or with the target at
the new head and the land recorded once. A land `Reconcile` records gets
its follow-up and its report (`concluded`) then, the hooks registered at
the composition being in place before the start hooks run; a land
recorded before the stop has no row left, so its follow-up is not run
again.

`Recover` (`recover.go`), once `Reconcile` ran and before the listener
binds, reads every attempt still `running` that has no `land_journal`
row left (one `Reconcile` kept may have moved its target, which only its
record may end), whatever its workspace, oldest first, and hands each to
the pool again (`hand`, the form of `submit` for
an attempt run again) with a deadline counted from then, not from its
creation; the pool starts them when `RunPool` runs, while requests are
served, and since no request was served before, no attempt is handed
over twice. Such an attempt runs through `rerun`: `readmit` checks the
land's preconditions again with `admit`, for its top change, `stack` for
an attempt covering more than one change, no expected version and the
attempt itself left out of the running-attempt check; a precondition that
no longer holds, such as a change abandoned while the hub was stopped,
ends it `failed` `internal` (`fail`), with its `parley.change.land_fail`
event. Otherwise `run` runs it as it runs a new attempt, with what the
attempt stores: its changes at their stored versions, its strategy,
trigger, session and requested override, and `requested_by`, the actor
of its events; its id and `created_at` do not change, and a target that
no longer exists ends it `failed` `internal` as in any run.

### Landing policy

`internal/hub/policy.go` adds two parts. `composePolicy` comes after
`composeSCM` and before `composeChanges` in `parts`, so that the start hook
`policy` binds the component before the change component's start hooks
record versions, which ask the landing evaluator. It creates a zero `policy.Component` (`Policy()`), adds
`policy.SchemaStep` (the step `policy_revisions`) and `policy.ReviewStep`
(the step `change_approvals_checks`), registers the component's evidence
source (`RegisterEvidence(comp.Facts())`), registers its `Routes()`
with `AddRoute`, registers `WorkspaceDeleted` as a remover of the
workspace teardown, which deletes a deleted workspace's revisions,
verdicts and check results in the delete's transaction, and binds the component in the start hook `policy`
to the store (`Store`), `policyWorkspaces` (`Workspaces`), an adapter over
`workspace.BySlug` read through the querier it is given, answering the
workspace's kind, status and default branch (`policy.Workspace`),
`policyUsers` (`Users`), an adapter over `identity.UserByUsername` on
`Store.Main()` that answers whether a user of that username exists, never
reading the value as an id or an e-mail
(`identity.ErrNotFound` is no, any other error is returned), the event
component (`Events`), the change component (`Changes`), an opener of the
workspaces' repositories (`Repos`), and the hub's clock and logger;
`AdjustPolicy` adjusters run before the bind. The component embeds
`policy.EvidenceSlot`, so `RegisterEvidence` is called on the hub's
instance, and a test's `Compose` or a test world replaces the source.

`composePolicyEvaluation` comes after `composeLand`. It registers the
component on the change component as its landing evaluator
(`RegisterLandingEvaluator`) and `policyGate` on the land component as its
policy gate (`RegisterGate`), adds the component's `VersionObserver()`
after the mergeability observer (`AddVersionObserver`) and registers
`ExpandApprovals` and `ExpandChecks` as the expanders `approvals` and
`checks` (`RegisterExpander`), all while the hub is composed, so that a
test's `Compose` replaces them.

The evaluation of a change (`evaluate`, `reevaluate.go`) reads, through
the querier it is given, the current revision of the policy of the change's
target workspace. That is revision 0 for a task fork, as the kind from
`Deps.Workspaces` says. It then runs `Evaluate` with `factsOf` the change
(`changeadapter.go`), the cause, the component's `Files` and the installed
evidence source.

- `Files` reads the change, then `DiffChanges(base_sha, head_sha)` in the
  repository of the change's target workspace. That repository holds every
  version head, a fork's included. It keeps `Path` and `OldPath` of every
  file, sorted and each once, with no cap.
- `EvaluateLanding(ctx, tx, ch, v, cause)` is the change component's
  evaluator. It runs inside the change component's transaction, reads
  everything through it, never opens `Store.Tx` (which would deadlock) and
  does not announce: the change component's callers do.
- `ReevaluateIn(ctx, tx, workspace, number)` reads the change. An unknown
  one is `ErrChangeNotFound`, the change component's
  `change.ErrChangeNotFound`, and a landed or abandoned one
  `ErrChangeClosed`, with nothing written. It reads the cause with
  `Changes.LandingCause` and evaluates. It then stores the result with
  `Changes.SetLanding` and runs `Changes.AnnounceLandable`, all in `tx`, and
  returns the result and the revision. `Reevaluate` runs it in a
  `Store.Tx` of its own, and reads the revision only inside it: write
  transactions run one at a time, so one that races a replacement either
  runs first, and the replacement's pass evaluates the change again, or
  after it, against the new revision.
- `reevaluateWorkspace(ctx, tx, workspace)` is the replacement's pass. It
  lists the workspace's draft and open changes with
  `Changes.DraftAndOpen` through `tx`, ascending, and runs `ReevaluateIn`
  for each. Landed and abandoned changes are not listed. The first error
  stops it and is returned without wrapping the cause, so that the
  replacement answers `500 internal` even for a refusal of the change
  component.
- `JudgeVersion(ctx, workspace, number, version, base, head)` judges the
  version a land lands, in one `Store.Tx`: it reads the change there (an
  unknown one `ErrChangeNotFound`, a landed or abandoned one
  `ErrChangeClosed`, whatever the version). When `version` is the change's
  current version it does what `ReevaluateIn` does, storing and
  announcing. Otherwise it evaluates the files `base..head` with the
  evidence of `version` against the current revision, with no cause, and
  stores nothing. Since the change is read in the transaction that judges
  it, a version recorded after the land read the change cannot be judged
  in place of the one it lands.
- `policyGate` (`internal/hub/policy.go`) answers the land's gate with
  `JudgeVersion` for the attempt's workspace, change number, version, base
  and head, and converts the result with `Result.Landing` into the
  `GateAnswer`.

The schema keeps `policy_revisions`: one row per stored revision, keyed by
`(workspace_slug, revision)` with `revision > 0`, holding the document in
normal form as JSON text, `updated_at` (Unix nanoseconds) and the writer
(`actor_type`, `actor_id`, `username`). It has no foreign key to the
workspaces. `CurrentRevision(ctx, q, slug)` reads the highest stored
revision, or revision 0 (`NeverSet`, no time, no writer) when none is;
`RevisionAt(ctx, q, slug, n)` reads revision `n`, and revision 0 only
while none is stored; `InsertRevision(ctx, tx, slug, rev)` stores a
revision with a positive number, a time and a writer. `Revision` marshals
as the routes answer it: `revision`, `default`, `rules`, `updated_at`
(whole seconds, UTC) and `updated_by`.

The step `change_approvals_checks` (`ReviewStep`, `review_schema.go`)
keeps the evidence, with no foreign key to the changes:
`change_approvals`, one verdict per row (`id`, the change and version, the
user's id and username, the credential's type and id, `verdict`,
`comment`, `carried_from`, `created_at` in Unix nanoseconds), unique on
`(workspace_slug, number, version, user_id)`, and `change_checks`, one
result per `(workspace_slug, number, version, name)` with the status,
summary, url, details as JSON text and the reporter. No table keeps who
created a version: the counting rule (`counts`, `counting.go`, a pure
function of the verdict, the requirement's approvers, the workspace's
owner and organization members and the versions' creators) reads each
version's creating credential from the change component
(`ReadVersions`, through `creatorsOf`), which records it with the version
and again when a raced write re-attributes it. Owner and organization come
from `access.RecordBySlug` and membership from `identity.ActiveOrgIDs`,
read through the caller's querier (`newCounter`).

- The routes (`approval_routes.go`, `check_routes.go`) read the change
  outside the transaction only to answer `change_not_found` before the
  body is judged; in one `Store.Tx` they check the workspace's status and
  the change's state (`writableChange`) and the version, write the row,
  run `ReevaluateIn` and `Emit` the event. A failure returns the error as
  it is, so the pipeline answers `500 internal` and nothing is committed.
  Write transactions run one at a time, so two verdicts of one user on
  one version, or two reports of one check, both succeed and leave one
  row; the unique keys are a backstop. The two lists confine a token bound
  to a task fork, on the fork's parent, to changes whose source is the
  fork (`readableChange`), with the constant `change.ErrChangeNotFound`.
- `Facts()` is the evidence source. Asked about one version
  (`EvidenceQuery.Version`, which `JudgeVersion` sets to an older version
  of a land), it answers the counted `approve` verdicts on that version,
  whether a counted `reject` is there, and the status of each named check
  whose result applies to it (`applying`, `check.go`: the version's own
  result, else the nearest one through `patch_equivalent_to`, walked by
  `equivalenceChain`; with `FreshChecks` the version's own only). It reads
  only the version, name and status of the named checks on the versions of
  that chain (`readCheckStatuses`); the list reads the results of the chain
  once and indexes them by name (`indexChecks`), so both grow with the
  results that may apply.
- `VersionObserver()` (`carry.go`) reads the recorded version as stored
  in the version's transaction, since the stack linker may have
  recomputed its `patch_equivalent_to`, copies every verdict of the version
  it names to the new one (new `id`, `carried_from` the source's
  `carried_from` or version), and runs `ReevaluateIn` when it copied any,
  because the change component stored the landing before telling its
  observers. A verdict recorded on an earlier version that the current
  version reaches is copied by the route to every version from the named
  one to the current one, replacing the user's verdict on each
  (`carryForward`).
- `ExpandApprovals` and `ExpandChecks` (`expand.go`) serve the includes
  with the lists of the routes (`listApprovals`, `listChecks`), read
  through `Store.Main()` since an expander is given no transaction.

`GET /workspaces/:slug/policy` is registered with the scope `changes:read`
and the need `read`, under the pattern of the contributor table, so the
scope, the binding guard and the access check answer before the handler.
The handler parses the `revision` query parameter (decimal digits only,
else `400 invalid_request`; digits beyond the range of `int64` read as
`math.MaxInt64`, a revision never stored, so `404`), reads the workspace's kind from the access
decision (`access.DecisionOf`), answers revision 0 for a task fork without
reading the store, else reads the current or the named revision through
`Store.Main()`, and writes the revision with the header `ETag` holding
its number in double quotes; a revision that is not found is
`404 not_found`.

`PUT /workspaces/:slug/policy` is registered with the scope
`workspaces:write`, the need `owner` and a body limit of 1 MiB; it is not
in the contributor table, so the binding guard refuses a fork-bound token
on the parent with `403 token_bound`. The handler refuses a task fork (from
the access decision) with `400 not_supported_on_fork` before reading the
body, then an `If-Match` that is not a revision (digits beyond the range
of `int64` are one, which never matches) and then a body that is not one
JSON object (unless it is over the document limit, which `Validate`
refuses as `document`) with `400 invalid_request`, then validates the
document with `Deps.Users` (`400 invalid_policy`). The rest is one
`Store.Tx`: the workspace record is read again and an archived one is
`409 workspace_archived`; `Replace` reads the current revision, answers
`ErrRevisionMismatch` (`412 revision_mismatch`) for another `If-Match`,
inserts the normal form as the next revision, written by the caller
(`identity.PrincipalOf`) at `Deps.Now`, and deletes the revisions numbered
50 or more below it; `Deps.Events` takes `parley.policy.update`
(`resource_type` `policy`, `resource_id` the slug, metadata `revision` and
`rules`); and `reevaluateWorkspace`, run last in the transaction,
evaluates every draft and open change of the workspace against the new
revision, stores each landing and runs the announcement for it. Any error
rolls all of it back: the revision, the event, every landing and every
`parley.change.ready`. Every listed change is measured and diffed inside
the write transaction, so other writers wait for the pass. `Store.Tx` runs write
transactions one at a time, so the read of the current revision and the
insert are one serialised step: concurrent replacements get consecutive
revisions, and of two with the same `If-Match` only the first is stored.

`POST /workspaces/:slug/policy/evaluate` is registered with the scope
`changes:read`, the need `read` and a body limit of 1 MiB, under the
pattern of the contributor table, so a fork-bound token reaches it on the
fork's parent. It opens no `Store.Tx`: every read goes through
`Store.Main()`, so an evaluation writes nothing. The handler decodes the
body into its raw members (`server.DecodeJSON`), and `parseEvaluate`
refuses with `400 invalid_request` a body without exactly one of `paths`
and `change`, a `change` that is not an integer, a `target_branch` given
with `change`, `paths` that are not 1 to 2 000 non-empty strings not
starting with `/` (counted as sent, then sorted and each kept once), and a
`target_branch` that is not a string of 1 to 255 characters. It reads the
stored revision as `GET` does (revision 0 for a task fork, from the access
decision). A `policy` member is validated by `Validate` with its raw JSON
text, whose length is the size the format limits (`400 invalid_policy`),
and replaces the stored document for the evaluation only. For `paths`,
the target branch is `target_branch` or the workspace's default branch
(`Deps.Workspaces`); none is `400 invalid_request`. For `change`, the
handler reads the change with `Changes.ReadChange` in any state, answers
`change.ErrChangeNotFound` (`404 change_not_found`, `no such change`) for
an unknown number and, with the same body, for a caller whose
`access.BindingOf` has the relation `parent_of_bound_fork` when the
change's source workspace is not the bound fork, and takes the files
between the change's base and head (as `Files` does) and its target
branch. `Preview` evaluates them with no evidence and no cause, and the
answer is `{policy_revision, status, requirements}` with the stored
revision. `Preview` takes the request's context, so a request its caller
abandons stops at the next file. The largest body (2 000 paths of 450
characters against 190 rules of one 255-character glob each) costs well
under a second of one core.

### Land rules

`internal/hub/landrules.go` adds the part `composeLandRules`, listed after
`composePolicyEvaluation` in `parts`, so that the change and land
components, the policy gate and the push rule engine all exist. It
registers, each on the instance it plugs into
([ADR 08](adr/08-register-the-push-collaborators-on-the-ref-writer.md)):

- the version-files lookup of the push rule engine, `land.VersionFiles()`,
  set on the composition (`versionFiles`) for `composeGitServer`'s start
  hook to bind as `pushrule.EngineDeps.VersionFiles`. Asked with the slug,
  the branch, the new head and the opens-change flag of an update that adds
  no commit of its own, it answers no version unless the branch is the
  source of a draft or open change (of the workspace, or of its parent for
  a task fork) or the update opens one; otherwise the files between the
  head and its merge base with the change's target branch, or the branch
  an open defaults to, read in the source workspace's repository;
- `PathGuard()`, as the change component's version guard (the manual
  restack's replayed version) and as the land component's first request
  guard (the current version of every change a land covers, bottom first,
  in the target's repository). Both answer `403 path_not_allowed` with the
  path, and both are `CheckPaths(ctx, repo, globs, base, head)`: the paths
  that differ between base (`""`, the empty tree) and head, both paths of
  a rename, deduplicated in byte order, held to `identity.MatchAny`; nil
  globs read nothing, and a git failure is an error, never a refusal;
- `OverrideGuard()`, the second request guard, and `PendingOverride`, the
  override rule (see [Landing](#landing));
- the automatic lander, `land.AutoLander`, as the change component's
  `auto_land` guard (`CheckAutoLand`) and as a final observer of the land
  component (`Finished`); its schema step `AutoLandStep` (the tables
  `land_auto_land`, one row per change whose flag is on with the retry
  count `retries`, the due time `due_at` of a pending start, NULL for
  none, and `held_by`, the change below that held a start back, and
  `land_auto_land_position`, the position in the event log it has read up
  to); its `WorkspaceDeleted(ctx, tx, slug)`, a remover of owned data on
  the workspace teardown that deletes the workspace's `land_auto_land`
  rows in the delete's transaction, so that a workspace registered again
  under the slug, whose changes are numbered from 1 again, inherits no
  retries, due starts or holds; the start hook `auto-land`, after `land-recover`, which binds it
  (`AutoLandDeps`: the store, the land component as `Starter`, the change
  component, the event component, the users through
  `identity.UserByID`, the access rule, the workspace records, the
  repositories, the clock and the logger; `AdjustAutoLand` adjusters run
  before the bind) and runs `Recover`; and the background work `auto_land`
  (grace 5 s), which runs `Run` until the stop.

The lander reads `parley.change.ready` events with `ReadAfter` from its
stored position (`Pass`, at most every `Interval`, 1 s, by default), and
starts nothing from any other sign of a landable change
([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)). For
each it runs the start procedure: nothing for a change that is not open,
without the flag, not landable, with a `running` attempt, held by an
unlanded change below, or with a retry due later; then the setter's
permission, read through `AutoLandSetter` (`mayLand`, the land route's
rule, taken out of `authorize`), and the setter's stored globs against
the current version; a failure clears the flag with `SetAutoLand`,
deletes the row and writes `parley.change.land_fail` (`not_permitted`) by
`system` in one transaction with the position; otherwise
`Start` with trigger `auto_land`, the version it checked as the expected
one, and `requested_by` the setter. The land engine writes the events of
an attempt with trigger `auto_land` by `system` (`emit`). The guard writes
a row due now in the transaction that turns the flag on, so that a change
landable already is started once that transaction commits; turning the
flag off deletes the row. `Finished` settles each automatic attempt in one
transaction (`landed`, `conflict`, `blocked`: count 0; `failed`: the next
due time, 2 s doubled per earlier retry up to 300 s, or, after the tenth
retry, the flag cleared), and, for every landed attempt whatever its
trigger, releases the changes it held: one already on the landed commit is
announced again (`ForgetLandable`, `AnnounceLandable`), so that its
`parley.change.ready` starts it. `Run` waits on the lander's `After`
clock until the next due time or `Interval`, whichever comes first, and
runs the due starts (`startDue`), dropping one whose change is no longer
to be landed with its count; `Recover` runs the starts already due and
then the start procedure for every open change with the flag on.

The revert route (`revertRoute`, in the land component's `Routes()`)
reads the change, the body, who may revert (`mayRevert`, the land rule),
the session, the workspace's state, the change's state and `reverted_by`,
the target branch and a path-scoped caller's paths (`CheckPaths` on the
landed version); it then builds the undo commits with
`gitrepo.(*Repo).Revert` by the landing identity, checks the open
(`change.CheckOpen`) before the branch exists, creates the branch through
`Deps.Rules` (`Evaluate`) and the ref writer (`Apply`, `Finish`, via `api`,
compare-and-swap against no ref, the next name on a lost race), opens the
change with `change.Open` (`KindRevert`, `Reverts`, `AfterMove`,
`SourceGranted`) and writes `parley.change.revert` in that transaction; an
open that still fails deletes the branch again through the ref writer.

### Awareness

`internal/awareness` shows agents the other work on their paths: an index
of the files each in-flight change touches, the overlaps it implies and
their announcement, the activity query, and claims. `composeAwareness`
(`internal/hub/awareness.go`) comes after the parts whose components it reads, since it reads
the change component, the event component, the access rule and the
workspace service the earlier parts create. The package imports neither
`internal/change` nor `internal/hub`: it declares what it needs in
`deps.go` and the part adapts the hub's components to it.

- `Deps.Events` is an `EventSource`, `ReadAfter(ctx, after, limit)`, which
  `*event.Component` satisfies as it is; `Deps.Emitter` takes events in the
  caller's transaction (`c.Events()`).
- `Deps.Changes` is a `ChangeReader`, `ReadChange(ctx, q, workspace,
  number) (awareness.Change, error)`, `ErrChangeNotFound` for none; the
  hub's adapter `awarenessChanges` maps `(*change.Component).ReadChange`
  onto the package's own `Change` (state, target, version, base and head,
  `depends_on`, session, landed commit and time, and the summary members of
  an overlap entry).
- `Deps.Repos` opens a workspace's repository with the hub's git;
  `Deps.DefaultBranch` reads a workspace's default branch
  (`workspace.BySlug`); `Deps.Owns` is the access rule's `CanWrite` (level
  owner), asked before a claim is renewed or released by someone other
  than its creator or the admin token.
- `Deps.Config` holds the three `[awareness]` keys (`ConfigKeys`,
  `ConfigFrom`, which a configuration check of the part runs, so a default
  longer than the maximum stops the start); `Deps.IndexInterval` (1 s) and
  `Deps.ExpiryInterval` (1 min) are the two background periods, which tests
  change through `(*Composition).AdjustAwareness`. `(*Composition).Awareness()`
  returns the component.
- The session interface, `Sessions` (`Check`, the session-linking rule, and
  `Record`, the claim link of a session), is registered on the instance with
  `RegisterSessions`. The sessions part registers it (see
  [Agent sessions](#agent-sessions)); without one every claim that names a
  `session_id` is refused `400` `unknown_session`.

The store schema is two steps. `SchemaStep` (`awareness_index`) creates
`awareness_changes` (per workspace and number: target branch, `phase`
`in_flight`, `landed` or `out`, landed commit and time, session),
`awareness_files` (workspace, number, path), `awareness_pairs` (the
announced pairs, workspace and the two numbers, lower first) and
`awareness_position` (the one row of the worker's position in the event
log). `ClaimSchemaStep` (`awareness_claims`) creates `awareness_claims`,
times in UnixNano, with `ended_at` set when a claim is released, expired
or fulfilled.

**The index worker** (`worker.go`, `index.go`) runs as background work
`awareness_index` (grace 5 s): `Run` calls `Pass` every `IndexInterval`, and
passes are serialised by a mutex, so a test's own `Pass` never races the
background one. A pass reads the log in batches of 100 from the stored
position and acts on `parley.change.version`, `parley.change.land`,
`parley.change.abandon` and `parley.change.restore` of resource `change`;
for each it reads the change and, for a version or a restore, lists the
files of its current version with one `DiffChanges(base or the empty tree,
head)` in the target workspace's repository, both before it opens a
transaction, so git never runs under the store's write lock. Then one
`Store.Tx` runs `apply` (the index update: files and target replaced and
in flight; landed with commit and time; out; in flight again), the claim
link or fulfilment, the overlap comparison and the event's `seq` as the new
position. Other events only move the position, stored once per batch. A
version whose files cannot be listed is indexed with none and logged with
the workspace and number; a change the reader no longer finds is skipped;
a transaction that fails leaves the position, so the next pass applies the
event again; a position the log has expired past
(`event.ErrPositionExpired`) is logged and the pass goes on from the
log's oldest event.

**Overlaps** (`overlap.go`, `entry.go`, `overlaps.go`). `Partners` gives
the other in-flight changes of the workspace and target with a file in
common, leaving out the changes of the same stack, which it decides at
every evaluation by following `depends_on` through the change reader to the
bottom change (nothing about stacks is stored, so a `depends_on` edit needs
no index update). At the end of every `apply`, the change's partners are
compared with the recorded pairs that include it: a new pair gets one
`parley.change.overlap` on each change (paths built with
`event.PathList`) and a row in `awareness_pairs`; a recorded pair that no
longer overlaps is deleted, so it is announced again when it returns.
`Overlaps(ctx, q, ws, number)` builds the ordered entries (`EntryOf`) for
`GET …/changes/:number/overlaps`, which pages them on the keyset `{count,
number}`, and for the `overlaps` include expander the part registers on
the change component.

**Routes** (`routes.go`): `(*Component).Routes()` returns the seven
routes, all declared with the access need `read`, the reads with the scope
`changes:read` and the claim writes with `changes:write`, the four reads
with the patterns of the contributor table of `internal/access`. Each acts
on `actedOn(r)` (`resolve.go`): the record the access check attached
(`access.DecisionOf`), or its parent for a task fork. The activity
(`activity.go`) matches files and globs with `identity.MatchPath`, the
path-limit matcher, and two globs with `canOverlap` (`glob.go`): either
one matches the other's text before its first `*`.

**Claims** (`claim.go`, `claim_create.go`, `claim_routes.go`). Every
answer renders a claim with `objectOf(claim, now)`, which reports a stored
`active` claim at or past `expires_at` as `expired`; the limits, the list's
state filter, the activity and the renewal use the same reported state. A
creation validates the body member by member, asks the session interface,
then counts the live claims of the workspace and of the session, inserts
the claim, records the session link and writes `parley.claim.create` in
one `Store.Tx`; its answer's activity is built after the commit, since it
runs git. A release writes `parley.claim.release` in the transaction that
sets `released`. The expiry run (`expiry.go`, `ExpireClaims`, background
work `awareness_claim_expiry`, grace 5 s) stores `expired` with `ended_at`
set to `expires_at` and writes `parley.claim.expire`, one transaction per
claim. The index worker links the active claims of a version-1 event's
session (not a retarget) and fulfils the active claims of a landed or
abandoned change (`claim_link.go`). The maintenance task `claim_cleanup`
(`RemoveEndedClaims`, `cleanup.go`) deletes claims that ended 7 days ago
or earlier, and `RemoveWorkspace`, registered on the workspace teardown
([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)),
deletes a workspace's claims and index rows in the delete's transaction.

### Agent sessions

`internal/session` keeps the agent sessions: the session record, its token
usage records and its links to the work done in it, in three tables of the
transactional store (`SchemaStep`, step `agent_sessions`: `sessions`,
`session_usage`, `session_links`). No table has a foreign key to
`workspaces`: a session, its usage and its links outlive their workspace,
and the session keeps the parent's slug of a fork it was opened on
(`parent_slug`), so that readers of the parent and the linking rule still
reach it after the fork is gone. Usage and links go with their session
(`ON DELETE CASCADE`). The package imports neither `internal/hub` nor any
package that takes a session id; it declares what it needs in
`session.go` and the hub part adapts the hub's components to it.

`composeSessions` (`internal/hub/session.go`) comes after every part whose
component it registers with. It adds the
schema step and the routes, and binds the component in its start hook
`sessions` with `session.Deps`: the store and the hub's clock, `Access`
(the access rule's `Decide` and `Readable`), `Workspaces` (the minimal
workspace record of a slug, `sessionWorkspaces`, read with
`access.RecordBySlug`), `Coverage` (whether a bound PAT's binding covers a
slug, `sessionCoverage`, from `(*access.Service).CoveredSet`; nil makes
nobody bound) and `Events` (the hub's event component). Tests change the
dependencies with `AdjustSessions`; the component is `Sessions()`.

- **One builder.** Every route answers the session object built by one
  function (`object`), which reads the usage records and the links when
  the answer is built: `token_summary` from `session_usage`, `forks`,
  `sandboxes` and `changes` from `session_links` in link order, each cut to
  500 entries, and `counts` as the true totals per kind.
- **One read rule.** `canRead` decides who reads a session (a bound PAT's
  binding must cover the session's workspace first; then the admin token,
  the creator, a reader of the workspace, a reader of the stored parent);
  `readableSession` checks the path id, reads it in lower case and answers
  the one `404 session not found`. `canChange` decides who may complete or
  report usage: the creator and the admin token.
- **One transition.** `endActive` is the conditional update `… WHERE status
  = 'active'` that both a complete and a forced close make, so that of the
  two only one takes effect. A usage report re-reads the status in the
  transaction that stores it.
- **The linking rule.** `(*Component).Check(ctx, caller identity.Principal,
  sessionID, slug)` returns the lower-case id or `ErrUnknownSession`, one
  value for every refusal, and any other error for a failed read; it never
  reads the session's status or the coverage, and writes nothing.
- **The links.** `(*Component).Record(ctx, tx, link)` stores one `Link`
  (built by `ForkLink`, `SandboxLink`, `VersionLink`, `PushLink`,
  `CommitLink`, `ClaimLink` or `LandLink`) in the caller's transaction; a
  repeated link is kept once, and a session that does not exist is
  `ErrUnknownSession`.
- **The forced close.** `(*Component).CloseActive(ctx, tx, slug, reason,
  at)` ends the active sessions of exactly that slug, in the caller's
  transaction, for the reasons `workspace archived`, `workspace deleted`
  and `fork expired`, and emits one `parley.session.force_closed` per
  session, with the stored parent as the event's `Parent`, since a fork's
  record may already be gone when a removal calls it.
- **The hand-off to the metrics.** `OnUsage` registers usage observers,
  called once per new usage record after its transaction has committed,
  and `ActiveCounts` counts the active sessions per workspace. No part
  registers an observer or reads the counts yet.

The part registers, through one adapter per consumer (`registerSessionLinkers`),
the linking rule and, where the consumer records links, the recording
operation, each written to the interface that consumer declares:
`forkSessions` on `c.Forks()` (fork links), `pushSessions` on
`c.PushOptions()` (check only), `scmSessions` on `c.SCM()` (check only),
`changeSessions` on `c.Changes()` (version links), `landSessions` on
`c.Land()` (land links, refusing with `change.ErrUnknownSession`),
`awarenessSessions` on `c.Awareness()` (claim links) and `sandboxSessions`
on `c.Sandbox()` (sandbox links). Each maps `session.ErrUnknownSession` to
its consumer's own refusal and every other error to a failure. A push and
an API commit become links through the change version they record. The
part registers `sessionCloser` as the session closer of the workspaces'
teardown set (ADR 07): an archive calls it, under its savepoint, for the
workspace and its forks, and `RemoveOwned` calls it for every workspace or
fork it removes, with the reason as text. It registers no remover of
session records.

### Agent telemetry

`internal/telemetry` keeps what agents report about their runs, in six
tables of the analytical store, one schema step each (`SchemaSteps`:
`telemetry_run_events`, `telemetry_session_outcomes`,
`telemetry_tool_calls`, `telemetry_tool_errors`, `telemetry_traces`,
`telemetry_postmortems`; tables `run_events`, `session_outcomes`,
`tool_calls`, `tool_errors`, `traces`, `postmortems`). Each kind is keyed by
its own `id`, so one UUID may name records of two kinds; each table has an
index on `(workspace, run_id, timestamp, id)` for the reads of a run in time
order, and `postmortems` a unique index on `(workspace, run_id)`. A client
timestamp is stored in a fixed-width form (`2006-01-02T15:04:05.000000Z`) so
that the column sorts as time does, and answered in RFC 3339 without
trailing zeros; JSON members are stored as JSON text and answered as raw
JSON. Nothing is written to the event log, and no workspace remover is
registered: telemetry outlives its workspace.

`composeTelemetry` (`internal/hub/telemetry.go`) is the last of the parts,
since it reads the session component. It registers the key
`[audit].write_timeout` (`ConfigKey`, read with `WriteTimeoutFrom`), the
schema steps and the routes, and binds the component in its start hook
`telemetry` with `telemetry.Deps`: `DB` (`telemetry.SQL` over
`Store.Analytics()`), the hub's clock and logger, `WriteTimeout`, and
`Sessions` (the session component, through `RunOf`). Tests change the
dependencies with `AdjustTelemetry`; the component is `Telemetry()`.

- **One gate.** Every ingestion route is built by `ingest` (`gate.go`):
  `Scope` `audit:write` and the access need `write`, so the pipeline's
  scope and access checks answer first; the handler then checks the path's
  `run_id`, runs the route's validator, refuses an archived workspace with
  `409 workspace_archived`, and acts. A write the write limit ends is
  answered `503 unavailable` with `Retry-After: 5`. The six single-record
  routes are creating `POST`s (`single`); the two batch routes are not.
- **One reader per kind.** `fields` (`validate.go`) reads a body member by
  member, a violation being `400 invalid_request` whose message starts with
  the member's name; `readHead` gives every kind but the postmortem its
  `id` and `timestamp` defaults (the postmortem's reader builds its own
  head, with a generated id and the hub's time, whatever the body sends),
  and the readers of run events and traces serve the single routes and the
  batches alike.
- **One write.** `(*Component).write` (`store.go`) runs one transaction of
  the analytical store under `context.WithTimeout` of the write limit and
  rolls it back on any failure; a failure whose limit's context has ended
  counts as a timeout whatever its text. SQLite waits for another
  connection's write lock in its busy handler without looking at the
  context, so `SQL`, the adapter over `*sql.DB`, begins each transaction
  on a connection of its own whose `busy_timeout` it cuts to the time left
  and restores before the connection goes back to the pool (a connection
  it cannot restore is discarded): the wait for the lock ends with the
  limit too. The analytical store's `BEGIN`
  takes the write lock (`IMMEDIATE`), so `storeOnce`, which reads an id and
  inserts the record only when it is new, stores each id once however
  many requests race: the others read what it stored. A replay is answered
  from the stored row, an id of another workspace or run is `409
  conflict`. The postmortem does the same keyed on `(workspace, run_id)`.
  `DB` and `Tx` are narrow interfaces, so that tests inject store faults.
- **Batches.** `batch.go` reads the array, judges each item with its
  kind's reader, and classifies and stores the valid ones in one `write`:
  accepted, duplicate (stored in this run, or earlier in the batch), or an
  error (stored in another workspace or run).
- **Sandbox use.** `composeTelemetry` registers an observer with
  `c.Sandbox().RegisterObserver` that turns a `sandbox.Operation` into one
  of `SandboxExec`, `SandboxAttach`, `SandboxUpload` and `SandboxDownload`
  (`sandboxUseOf`; an exec's `Result` is dropped) and calls
  `(*Component).RecordSandboxUse`. It reads the session's run id with
  `RunOf`, stores nothing without one, and otherwise writes one run event
  under `context.WithoutCancel` and the write limit, without an access or
  status check; a failure is one log line naming the workspace, the
  sandbox and the event type. The operation values have no field for what
  an operation carried.
- **Run event observers.** `OnRunEvent(RunEventObserver)` registers a
  function told the `event_type` of every run event newly stored, by a
  single route, a batch or the recorder, after the write has committed
  (`observer.go`); nothing else is reported. An observer that panics is
  logged with the event type and the next one is called, so the answer
  does not depend on it. No part registers one yet:
  the metrics of the audit records will.

### Carry-patch workspaces

`internal/carrypatch` keeps what a `carry_patch` workspace adds to a
workspace: its upstream, its integration branch and its ordered list of
patch branches. `composeCarryPatch` (`internal/hub/carrypatch.go`) is the
last part of `parts`. It creates the `*carrypatch.Component`, adds
`carrypatch.SchemaStep` (`carry_patch_patches`), registers the component's
`Routes()`, and registers on the workspace service, which the package
cannot be imported by (ADR 01, ADR 07):

- `PostClone` as the post-clone step of `workspace.ModeCarryPatch`
  (`RegisterPostCloneStep`);
- the component itself as the push route's `PatchBranchLookup`
  (`IsPatchBranch`: a patch of the branch exists that is not deleted);
- `carrypatch.RemoveWorkspace` as a remover of owned data, so a delete
  removes a workspace's patches in its transaction.

It also adds the start hook `carry_patch_recovery`, which discards what an
interrupted rebuild left in the copies, and the maintenance task
`patch_purge` (see below).
- the component itself as the sync's `SyncExtension` and its
  `ExcludedBranchProvider` (`RegisterSyncExtension`,
  `RegisterExcludedBranchProvider`); the start hook sets
  `Deps.BranchMoves` to the workspace service, so the sync's patch-branch
  moves reach its branch-move observers.

Its start hook `carry_patch` binds `carrypatch.Deps`: the store, the secret
component (`Secrets`, a `workspace.SecretReader`), the hub's git
(`gitFor(c.env)`), `RepoPath` of the workspace service, its settings
registry, the event component, the clock and the logger, after the
adjusters of `AdjustCarryPatch`. It runs before the clone workers launch,
so no clone runs the step on an unbound component.

**The git runner.** Every git operation of the package runs through
`carrypatch.RunGit(ctx, limit, cred, op)`: the operation gets its own
deadline, `DefaultGitDeadline` (10 minutes) or the caller's shorter
`limit` (`DefaultBranchFetchDeadline`, 2 minutes, for a single-branch
fetch); when it passes, the context's end kills git's process group and the
error matches `ErrGitTimeout` and `gitrepo.ErrDeadline`. The text of any
other failure is cleaned with `gitrepo.SanitizeGitText` (the credential's
values are removed first, then URL user information becomes `***@`, so the
result is the same when it is cleaned again) while `errors.Is` still
matches the cause. `RequestError(err)` maps an operation the runner cut
off to `500 internal` (`a git operation did not finish within its time
limit`) for a request handler and passes any other error through.
Underneath, `internal/gitrepo` refuses, before git starts, a remote whose
URL is not `https://`, `ssh://` or `[user@]host:path` (`CheckTransport`,
an error matching `ErrTransportRefused` that names the transport, run by
every process with a remote URL: `ListRemote`, `Clone`, the fetches and
the pushes) and runs every such process with
`GIT_ALLOW_PROTOCOL=https:ssh`, after `Git.Env`, so a redirect or a remote
helper cannot leave those transports; and the operations carry-patch code
calls with a caller's value (`IsAncestor`, `LookupRef`,
`CompareAndSwapRef`, `SetRef`, `DeleteRef`, `FetchOriginBranch`) refuse a
value that begins with `-` with an error matching `ErrInvalidArgument`
before git starts, and hand git the others after `--end-of-options`;
`ResolveBranch` answers such a branch as not found, with no git started.

**The upstream.** `ResolveUpstreamCredential(ctx, secrets, slug)` reads
`UPSTREAM_GIT_PAT`, then `UPSTREAM_GIT_USERNAME` with
`UPSTREAM_GIT_PASSWORD`, then falls back to
`workspace.ResolveOriginCredential`, at every call; a read error is an
error, never no credential. `(*Component).FetchUpstream(ctx, w)` resolves
it (an error starts no git), lists the upstream (`Git.ListRemote`) for its
default branch, fetches with `Git.FetchRemote(ctx, dir, url, "upstream",
cred, deadline)` (every branch into `refs/remotes/upstream/*` through a
private namespace, pruned, no tags, no remote configuration), and then sets
or unsets the copy's `parley.upstreamDefaultBranch`
(`Repo.UnsetConfig`); a failure changes no ref and not the recorded
branch. The three share one deadline, `DefaultGitDeadline` or the shorter
`Deps.UpstreamFetchDeadline`, set when the call starts: each runs through
`RunGit` with what the earlier ones left of it, and a passed deadline is
`ErrGitTimeout`. `UpstreamBase(ctx, w)` reads only the copy:
`refs/remotes/upstream/<recorded default>`, else
`refs/remotes/upstream/<default_branch>`, else `ErrNoUpstreamBase`.

**The set-up.** `IntegrationBranch(w)` is the stored `integration_branch`,
or `DefaultIntegrationBranch` (`deploy`) while it is null, the one place of
that fallback. `Setup.Run` runs four steps behind the interfaces
`RecordWriter`, `UpstreamFetcher` and `CopyWriter`, each whatever the
earlier ones did: the conditional `UPDATE workspaces SET
integration_branch = 'deploy' … WHERE integration_branch IS NULL`, which
leaves `updated_at`; `FetchUpstream`; `rerere.enabled` `true`; and the
integration branch at `UpstreamBase`, or at `head_sha` when the fetch
failed or no base resolves. Its error names every failed step (`carry-patch
set-up of <slug>: …`) and matches `ErrUpstreamFetch` when the fetch was
among them, `ErrNoUpstreamBase` when only the base was missing.
`PostClone(ctx, w, repo)` is the replacing form, given the record and the
copy by the clone work: it writes the branch with `SetRef`, so a branch of
that name the clone brought from origin is replaced, and the clone logs its
error. `EnsureSetup(ctx, w)` is the form for a ready workspace: it
creates the branch only when absent (`CompareAndSwapRef(ref, id, "")`,
`ErrRefMoved` leaving it), never moving one. `Setup.LeaveBranch` leaves
the branch step out, as a rebuild's and a sync's repeat of the set-up do:
the rebuild writes the integration branch at its publish alone, the sync
creates a missing one at the base once it resolved
(`createIntegrationBranch`). Every repeat of the set-up of one workspace
holds the component's per-workspace set-up lock (`Component.setups`, a
`slugLocks` whose wait ends with the caller's context), so that two
upstream fetches never run into one copy at once, where they would fail
on each other's ref and configuration locks.

**The patch list.** The `patches` table holds a row per patch (`Patch`):
UUID id, `workspace_slug`, `branch_name`, `position` (NULL exactly when
`status` is `deleted`), `status`, `conflict_files` (JSON), the free-text
members, the times as UnixNano integers and the three origin fields. A
partial unique index over `(workspace_slug, branch_name) WHERE status <>
'deleted'` makes one branch one live patch; positions carry no index, so
`ShiftPositions` moves them with one `UPDATE`. The store functions
(`InsertPatch`, answering `ErrPatchExists` for that index, `ReadPatch`,
`LivePatchOfBranch`, `DeletedPatchOfBranch`, `ListLivePatches`,
`ListDeletedPatches`, `ShiftPositions`, `CountLivePatches`, `DeletePatch`,
`RemoveWorkspace`) take the caller's transaction or a querier.
`ObjectOf(p, replacedSHA)` is the patch object.

**The routes.** `Route(kind, rt)` declares a route's scope (`patches:read`
or `patches:write`) and access need, and attaches its kind (read or
write) for `(*Component).Guard(r)`, which a handler calls after its own
validation: it reads the record the access decision names with
`workspace.BySlug`, answers `400 workspace_mode_mismatch` for a mode other
than `carry_patch` (a fork's mode is always `standard`), and for a write
route applies `workspace.RequireReady`. The reads (`read.go`) page one
total order, `{d, p, t, i}`: the live patches by position, then the deleted
ones by `deleted_at` descending and id, up to `Limit+1` rows per page from
one store query, or from two when the page runs from the live patches into
the deleted ones (the second asks only for the rows the first left); the
single read adds `replaced_sha` from
`refs/parley/replaced/<branch>` (`ReplacedRefPrefix`) when it resolves. The
add (`add.go`, `batch.go`) decodes one object or an array, validates every
element before the guard, checks every branch against the live patches
before any resolution, resolves each branch in order with
`(*Component).ResolveBranch(ctx, w, branch, skip)` (`local`,
`origin_tracking` by `CompareAndSwapRef(ref, id, "")`, `origin_fetch` by
`Git.FetchOriginBranch`, which fetches one branch into
`refs/remotes/origin/<branch>` and answers `gitrepo.ErrNoSuchBranch` for a
branch origin lacks, or `skipped`; `ErrRefMoved` keeps a branch another
writer created meanwhile), and then, in one `store.Tx`, purges deleted
patches of the branches, places and inserts the patches and emits one
`parley.patch.create` each (ADR 05). The store's one write transaction at a
time orders concurrent adds; no other lock is held. Backup refs of purged
patches are deleted after the commit.

**Maintaining the list.** The update (`patch_update.go`), the delete and
the restore (`patch_remove.go`) and the reorder (`patch_reorder.go`) check
the body's shape, call the guard, and then do everything else in one
`store.Tx`: read the patch or the live list, check it, write, and emit
their event (`parley.patch.update`, `.delete`, `.restore`, `.reorder`); an
event that cannot be written rolls the change back. Every change of the
order goes through `renumber(ctx, tx, order, now)` (`patch_order.go`),
which gives the live patches in their new order the positions 1 to N and
stamps `updated_at` only on those whose position changes. The store runs
one write transaction at a time, so concurrent position changes are
serialized by that alone and the order never shows a gap or a duplicate.
A delete sets `status` and a NULL `position` in one `UPDATE` (the table's
check ties them) before renumbering the rest; a restore sets `active` and
position N+1 in one `UPDATE` and moves nothing else. No git ref is
touched by these four.

**The reset to origin.** `patch_reset.go` reads the patch (only `active`,
`conflict` and `disabled` are reset), fetches its one branch with
`fetchBranch` (the origin credential read now, `Git.FetchOriginBranch`
through `RunGit` under `DefaultBranchFetchDeadline`; the missing-branch
answer is the caller's, `missing_on_origin` here and `branch_not_found` for
the add), compares the hub's tip with origin's (`LookupRef`,
`IsAncestor`) and takes the action `created`, `none`, `fast_forwarded` or
`replaced` (`compareWithOrigin`, shared with the sync's refresh). For
`replaced` it first saves the hub's tip with
`WriteBackupRef(ctx, repo, branch, id)` (`backup_ref.go`), which with its
compare-and-swap form `SwapBackupRef(ctx, repo, branch, id, was)`, the
sync's, is the one writer of `refs/parley/replaced/<branch>`; then it
moves the branch with `CompareAndSwapRef` against the tip it compared
(`""` for `created`); `swapToOrigin` is these two writes for both. A lost swap (`gitrepo.ErrRefMoved`)
is `409 ref_moved` with the branch's current commit. From the choice of
action on, the reset runs on `context.WithoutCancel`. The record and the
moves are one `store.Tx`: it reads the patch again and refuses one now
`merged_upstream` or `deleted` (`409 patch_status_invalid`), records the
origin fields (when `patch_branch_source` is `origin`), emits
`parley.patch.reset` and, for `replaced`, `parley.patch.replace`, and
only then writes the backup ref and swaps the branch, so that a failed
record moves nothing and a lost swap rolls the record back. The git work
held in the store's write turn is two local `update-ref`s at most. When
the commit itself fails after the swap, the branch is swapped back. The
test-only `Deps.BeforeRefSwap(slug, branch)` runs after the compare and
before that transaction, for every action.

**The rebuild trigger.** `RebuildEnqueuer` (`EnqueueRebuild(ctx, slug,
RebuildOptions) (RebuildResult, error)`) is the interface every source of a
rebuild calls; the component holds a default that queues nothing, which
`RegisterRebuildEnqueuer` replaces and `Rebuilds()` returns. A reset that
moved the branch of an `active` or `conflict` patch calls it once its
outcome is recorded, when the workspace's `auto_rebuild_after_sync`, read
from the record again then, is on; `SubmittedBy` is the caller's user id
(the admin token's actor type for the admin token). A result that is not
`Queued` and an error (logged) leave the reset in place and report no
job.

**The sync.** The component is package 12's sync extension and its
excluded-branch provider. `ExcludedBranches` (`excluded.go`) names the
branch of every patch of `ListLivePatches`, the store's error unchanged.
`AfterFetch` (`sync.go`) holds the set-up lock and runs the set-up with
`LeaveBranch`, whose upstream fetch failure (the step matching
`ErrUpstreamFetch`) it returns alone, logging other failed steps, then
resolves the base with `UpstreamBase` (no base is an error) and only then
creates a missing integration branch at the base. `BeforeCompletion` cannot take state from the first stage, so
`startSyncPass` resolves the base again and computes `upstream_advanced`
and `force_push_detected` (`Repo.Missing` before `IsAncestor`, since a
stored head the copy lacks counts as forced), then runs, each on success
of the one before: `refreshFromOrigin` (`sync_refresh.go`), `detectMerges`
(`merge_detect.go`), `clearOriginFields`, `triggerRebuild`, and
`addition`, which marshals the eight members into `SyncAddition.Members`
and sets `UpstreamHeadSHA` only when upstream advanced. The refresh reads
origin's head from `SyncRun.OriginHeads` (this sync's fetch); syncs wait
for one another only during their origin fetch, so when the hub's tip
would be replaced it compares the tip again with the copy's
`refs/remotes/origin/<branch>`, which a later sync's fetch may have moved
and that sync's refresh moved the branch to, and takes `none` or
`fast_forwarded` towards that head when the hub's tip is it or its
ancestor, never moving a branch back. For a replace it reads the backup
ref at the comparison. It calls `Deps.BeforeRefSwap` before a moving
outcome and records each outcome in one `Store.Tx`: the patch read again,
the origin fields, the events, `SwapBackupRef` for a replace (expecting
the backup read at the comparison), and the compare-and-swap last, so a
lost swap, of the branch or of its backup, rolls back the fields and
events, a branch swap lost keeps the backup written; a commit that fails
after the swap puts the branch back. A move is told to `Deps.BranchMoves`
after the commit. A refresh error stops the walk with no retry; a failure
of the refresh, `detectMerges` or `clearOriginFields` runs
`rebuildAfterFailure`, which queues a rebuild when an `active` or
`conflict` patch's branch had moved or a patch was marked merged. Detection uses
`gitrepo.(*Repo).Cherry` and `Subjects` (`internal/gitrepo/cherry.go`,
full 40-hex ids only, so no revision is read as an option), through
`RunGit` like every git call of the package, and marks a merged patch in
one transaction with its `parley.patch.update`. `queueRebuild` goes
through `Rebuilds().EnqueueRebuild` with nil strategy and fail mode, so
the settings apply; a result that is not queued (a duplicate) reports no
job, and another error fails the second stage.

**Push control.** Four pieces decide and follow up every client write of a
`carry_patch` workspace's integration branch and patch branches, each
built by the component and reading the record, the effective settings and
the live patch of a branch through the interfaces `RecordReader`,
`SettingsReader` and `PatchReader` (the component implements all three;
tests replace them), read at every use:

- `PushControl` (`pushcontrol.go`, `(*Component).PushControl()`), the
  `pushrule.Controller` of rule 4: the integration branch
  (`IntegrationBranch(w)`) is refused before the settings or the patch list
  are read; a ref outside `refs/heads/` and a branch with no live patch are
  allowed; a patch branch is allowed with `patch_branch_source` `hub`,
  refused with origin's URL without user information with `origin` and no
  `push_patches_to_origin`, and with forwarding its delete is refused and
  its create or update allowed after a forward, a `pushrule.Forward` that
  calls the `Forwarder`. A failed read is an error, which the engine
  refuses as `internal`.
- `(*Component).Forward(ctx, repo, w, branch, commit)` (`forward.go`), the
  forward: it reads the origin credential (an error is `failed to resolve
  origin credentials`, origin not contacted), pushes the commit without
  force with `(*gitrepo.Repo).Push` from `repo`, the request's repository,
  whose git environment reads a git push's quarantined objects (nil opens
  the hub's copy), through `RunGit` under `ForwardLimit()`
  (`Deps.ForwardLimit`, else `DefaultForwardLimit`, 120 s), maps the
  failure to `origin rejected push: <reason>` (origin's hook text from
  `PushError.OriginMessage`, else a fixed reason per class), scrubbed, and,
  called without a repository (the landing guard), records origin's head
  (`SetOriginHead`) of a commit the copy holds; a write's forward leaves
  the record to the trigger.
- `Trigger` (`trigger.go`, `(*Component).Trigger()`): `AfterUpdates(ctx,
  slug, updates, source, sessionID)` runs, one request of a workspace at a
  time (`Component.triggers`, a per-slug lock apart from `setups`), on a
  `carry_patch` workspace whose updates created or moved a live patch
  branch: one `EnqueueRebuild` with `submitted_by` `system:push` or
  `system:land` when `auto_rebuild_after_push` is on (a duplicate is
  silent), then, with `hub` and `push_patches_to_origin`, `Trigger.Mirror`
  (`mirror.go`): the heads and recorded origin heads read once, each branch
  force-pushed under the lease of its recorded origin head, recorded on
  success, and `EventPatchMirrorFailed` (`parley.patch.mirror_failed`)
  written in a `Store.Tx` of its own for each branch that fails, unless the
  caller's context ended. A failure or panic is logged; nothing is returned.
  `PostPush` is its post-push listener: `via` `git` or `api` is the source
  push, `land` is ignored; it first records origin's head of each accepted
  forwarded update.
- `LandGuard` (`landguard.go`, `(*Component).LandGuard()`), a
  `land.TargetGuard` answering a land onto a patch branch by the same
  table, forwarding the new head with a nil repository.

`composeCarryPatch` registers `PushControl()` with
`c.PushRules().RegisterController` (and keeps it, `Composition.PushControl()`,
for tests to wrap), the trigger's `PostPush` with
`c.RefWriter().RegisterPostPushListener`, `LandGuard()` with
`c.Land().AddTargetGuard`, and an after-landing hook that starts
`AfterUpdates` with the source land on a goroutine of its own for a land
that moved its target. Both trigger paths run under a context that also
ends when `c.ShuttingDown()` closes (`untilShutdown`).

### Durable jobs

`internal/jobs` is the hub's durable job queue. It knows a job's type, its
workspace and two opaque JSON documents, the payload fixed at enqueue and
the result the attempt writes; what a job means is the business of the
handler registered for its type. Rebuilds (`internal/carrypatch`, type
`rebuild`) are its one type so far.

**The part.** `composeJobs` (`internal/hub/jobs.go`) comes right before
`composeCarryPatch` in `parts`. It creates the `*jobs.Queue`, which
`(*Composition).Jobs()` returns from the composition on, adds
`jobs.SchemaStep` (the table `jobs`, times in UnixNano, indexes for "the
pending jobs of a type and workspace" and "a workspace's jobs newest
first"), registers `q.RemoveWorkspace` as a remover of owned data (ADR 07),
and adds the start hook `jobs`, which builds `jobs.Deps` (the store, the
hub's clock as `jobs.SystemClock(rt.Now)`, the logger), applies the
adjusters of `AdjustJobs`, binds them and runs `Recover`, and the
background work `jobs` with the grace `jobs.ShutdownGrace` (30 s), which
runs `Run`. A handler is registered with `c.Jobs().Register(type, h)`
from a part or from `Options.Compose` ([ADR 09](adr/09-run-durable-jobs-on-a-queue-the-hub-composes.md)); a type without a handler
keeps its jobs `queued`.

**Enqueue.** `Enqueue(ctx, tx, type, slug, payload)` stores a `queued` job
(a `uuid.NewV4` id) in the caller's transaction, after checking in the
same transaction that no job of that type and workspace is `queued`,
`running` or `failed`; one that is is reported as `*jobs.DuplicateError`
naming it. `Store.Tx` admits one write transaction at a time, so the check
and the insert are atomic without a unique index (which the follow-up,
a queued job beside a running one, would break). `EnqueueFollowUp(ctx, tx,
type, slug, runningID, payload)` queues the one follow-up a running job
may have; a `runningID` of another type or workspace is
`jobs.ErrNotFound`, so a follow-up is always of the job it names. Both
poke the dispatcher.

**The dispatcher.** `Run(ctx)` makes a pass at once, then every
`Deps.Interval` (1 s) on the queue's `Clock` and whenever it is poked (an
enqueue, a requeue, a registration, every settlement). `Pass(ctx)` returns
the `failed` jobs whose `next_attempt_at` has come to `queued`, then claims
`queued` jobs of the types with a handler, oldest first (`created_at` in
whole seconds, then id), skipping a type and workspace that has a job
`running`, while fewer than `MaxRunning` (4) attempts run; each claim is a
conditional `UPDATE … WHERE status = 'queued'` that also empties the
result, inside one `Store.Tx`, and the attempts start on goroutines of
their own once it has committed. An unprompted pass first looks for work
on a read connection, with the same condition as the claim (a queued job
behind a running one of its type and workspace is no work), and opens no
write transaction without it; a poked
pass goes straight to the write transaction, since the poke may come from
a transaction still open, whose job only a transaction that waits for it
can see.

**Settlement.** `settle.go` records an attempt's outcome in one
`Store.Tx`, only while the job is still `running` (a job removed or
abandoned in between is left alone): `nil` is `completed`; an error is
retryable unless `jobs.Permanent` marked it, and `jobs.WithDetail`
attaches the detail document; a retryable error below `MaxRetries` (20)
is `failed` with the backoff `min(2 s × 2ⁿ, 2 h)`, and otherwise the job is
`dead_letter`; a panic of the handler is a retryable error naming it. The
handler's `Settled(ctx, tx, job, willRetry)` hook runs in the same
transaction under a savepoint: an error or a panic rolls back to the
savepoint, is logged with the job id, and the settlement commits. A
settlement whose own transaction fails is logged with the job id and not
retried: the job stays `running` until the next start's `Recover`.
`SetResult(ctx, id, doc)` replaces the result of a running job.

**Start, stop and removal.** `Recover` returns every `running` job to
`queued` before the first pass. At the stop `Run` claims nothing more,
settles the attempts that end within `ShutdownGrace` on the queue's clock
and waits, within the same grace, for the removals' cancellations under
way (below), since both write to the store the hub closes after `Run`
returns. At the end of the grace it cancels the attempts still running and
returns without settling them, so their jobs stay `running` for the next
start, but first waits for the settlements already under way; it starts no
goroutine of its own, so an attempt that ignores its cancellation leaves
nothing of the stop behind. `RemoveWorkspace(ctx, tx, slug)` deletes the
workspace's jobs in the delete's transaction and, since a remover cannot
see the commit, cancels the attempts of the removed jobs from a store
transaction of its own, which runs only after the delete's has ended.
`Depth(ctx)` counts the jobs of each registered type by status, the
source of `parley_jobqueue_depth`. `Cancel(ctx, tx, id)` and
`Requeue(ctx, tx, id)` serve the rebuild routes.

**Rebuilds.** `internal/carrypatch` implements its `RebuildEnqueuer` with
`(*Component).EnqueueRebuild`, which `composeCarryPatch` registers with
`RegisterRebuildEnqueuer`; the queue reaches the component as
`carrypatch.Deps.Jobs`. One unexported operation, `enqueueRebuild`, serves
it and the route `POST …/rebuild`: in one `Store.Tx` it counts the
`active` and `conflict` patches (the route alone refuses none with
`no_active_patches`), resolves the strategy and fail mode against the
settings, enqueues the payload `{strategy, fail_mode, integration_branch,
submitted_by, session_id}` and writes `parley.rebuild.enqueue`; a
duplicate stores and writes nothing. `RebuildRecordOf(job)`
(`rebuild_record.go`) builds the job record of the five job routes
(`rebuild_routes.go`), declared with `carrypatch.Route` and the scopes
`rebuilds:read` and `rebuilds:write`; every one of them looks a job up
with `rebuildJob` after the guard.

**Rebuild execution.** `composeCarryPatch` registers the component's
handler, `(*Component).RebuildHandler()` (`rebuild_outcome.go`), with
`c.Jobs().Register(carrypatch.RebuildJobType, …)`, and fills the rebuild's
`carrypatch.Deps`: `CommitterName`/`CommitterEmail` from
`rt.Config.Land`, `ConflictFiles` (`carrypatch.GitConflictFiles{}`, the
describer of a report's files) and `Scrubber` (`rt.Scrubber`); the test
seams `Push`, `FetchRemote`, `RebuildGitDeadline`, `RebuildPushDeadline`
and the hooks `BeforeRebuildPatch`, `BeforeRebuildPublish` and
`BeforeRebuildSwap` are left zero. `Run` calls `runRebuildAttempt`
(`rebuild.go`), whose `rebuildRun` holds the attempt's state and runs its
steps in order, each returning nil to go on or the attempt's outcome
(completed, retryable, permanent, or ended when the workspace is gone):
`start` (the workspace check through `admit`, the set-up repeated with
`LeaveBranch`, so the integration branch is not written before the
publish, `UpstreamBase`; a set-up that fails while the attempt's context
is cancelled is `shutdown`, not `upstream_fetch_failed`), `listPatches` and `build` (`rebuild_build.go`), `publish`
(`rebuild_publish.go`), `push` (`rebuild_push.go`), `complete`, then
`followUp` (`rebuild_followup.go`) after a completion or a `fail_fast`
conflict. Every git operation runs through `RunGit` under
`DefaultGitDeadline`; every store of the job's result is `jobs.SetResult`
of the whole `rebuildResult`, so the result is kept in memory and written
after each patch. `Run` maps the outcome to `nil`, a plain error,
`jobs.Permanent` or `jobs.WithDetail(jobs.Permanent(…), report)`, its text
and reason scrubbed with the attempt's credential values, and keeps it,
without those values, by job id for `Settled`, which writes `parley.rebuild.complete` or `parley.rebuild.fail`
and `parley.rebuild.followup` in the settlement's transaction; a
settlement without a kept outcome (a panic) is reason `git_error`.

The build works in a scratch working tree of `internal/gitrepo`
(`scratch.go`): `(*Repo).AddScratch(ctx, name, commit)` adds a detached
working tree at `<git dir>/parley-rebuild/<job id>` (`git worktree add
--detach`, no ref), whose `Pick` replays one commit (`cherry-pick
--no-commit`, then `CreateCommit` with the original author and the given
committer), `Merge` merges one commit with a merge commit (`merge
--no-commit --no-ff`, then `CreateCommit`), `Learn` redoes a merge commit
to record its resolution, `Reset` returns to a commit, and `Remove`
deletes it; `(*Repo).DiscardScratches` removes every leftover and prunes
the working-tree list. Every scratch git process runs with
`core.hooksPath=/dev/null`, `attr.tree` the empty tree (no filter, merge
driver or eol conversion from the content), `gc.auto=0`, and git rerere on
with automatic staging set per command (`rerere.go`), never in the
repository's configuration file; the resolutions live in the bare
repository's own `rr-cache`, so they are per workspace and go with a
reclone. A conflict's id↔path pairs are read from `MERGE_RR` before the
tree is reset. `(*Repo).ConflictFiles(ctx, base, target, commit, paths)`
(`trial.go`) describes a report's files with `git merge-tree` over the
three-way base the caller names: `BuildConflictReport` gives the first
parent of the commit that did not apply for `rebase`, the merge base of
the source and the target for `merge` (`PatchConflict.Merge`). `BuildConflictReport` (`rebuild_conflict.go`)
builds the report, `ConflictReport` (`patch_id` plus the hub's
`gitrepo.ConflictReport`), which the rebuild preview shares.

**Rollback and preview.** Two more routes of `rebuild_routes.go`.
`rollbackRoute` (`rollback.go`), a write route, looks the job up with
`rebuildJob` after the guard, tests its recorded
`previous_integration_head_sha` with `(*gitrepo.Repo).ObjectInfo` (a
commit object, the full id checked before git runs), reads
`refs/heads/<IntegrationBranch(ws)>` with `LookupRef`, calls the test hook
`Deps.BeforeRollbackSwap`, and moves the branch with `CompareAndSwapRef`
against the head read (`""` when the branch is missing). Only after the
swap won does it write `parley.rebuild.rollback` in a `Store.Tx` of its
own; when that transaction fails it swaps the branch back from the target
to the head read, logging an undo that fails. From the swap on it runs
under `context.WithoutCancel`, so a caller who leaves cannot leave a moved
branch without its event. `previewRoute` (`preview.go`) is declared a read
route (`rebuilds:read`, read access), and calls `workspace.RequireReady`
itself after the guard, since the guard serves read routes in every state.
It takes `UpstreamBase` (`ErrNoUpstreamBase` is `409 upstream_unknown`),
lists the patches with `ListLivePatches`, reads every tip in one
`RefValues`, and walks them with a predicted state: each tip is merged
onto it by `(*gitrepo.Repo).TrialMerge` (`merge.go`): `mergeTree`'s
`git merge-tree --write-tree` from every merge base of the two, as the
rebuild's `git merge` merges (with no `--merge-base`, so that a
criss-cross history is merged as git merge merges it), or from an
empty-tree commit when `MergeBase` finds none; it needs no working tree
and never consults `rr-cache`. A clean merge becomes the state through
`CreateCommit` (parents the state and the tip, the landing identity of
`committer()`); a conflict is reported with `BuildConflictReport`
(`Merge: true`). `Deps.TrialMerge` replaces the trial merge in tests.
Every git operation of both runs through `RunGit`; a git error that is
not a conflict is the whole preview's error, `500 internal`.

The remembered-resolution entries are the table `rerere_entries`
(`workspace_slug`, `id` (the `rr-cache` directory), `path`,
`recorded_at`), added by its own schema step
`carrypatch.RerereSchemaStep` (`carry_patch_rerere_entries`); an entry is
inserted when git records a conflict that has none, and its
`recorded_at` is moved when a resolution is learned; `resolved` is read
from git. `(*Component).RerereEntries`, `CountRerereEntries` and
`RemoveRerereEntry` are the reads and the removal the maintenance of
remembered resolutions builds on. `RemoveRerereEntries` is a remover of
owned data (ADR 07), and `PostClone` deletes the workspace's entries
before the set-up, since a fresh clone starts with an empty `rr-cache`.
An entry whose copy is missing or unreadable is listed unresolved.

**Remembered resolutions, patch status, recovery and purge.**
`rerere.go` adds the list (`GET /workspaces/:slug/rerere`, built on
`RerereEntries`, sorted newest `recorded_at` first, then `id`, and paged
in memory with a cursor of the two) and the forget (`DELETE
/workspaces/:slug/rerere/*pathspec`, `EmptyCatchAll`), each declared with
`Route` and its scope set to `workspaces:read` or `workspaces:write`, as
`rebuildRoute` does for the rebuilds. The forget validates the pathspec
before `Guard`, selects the entry whose id equals it, else every entry
whose path does, and removes each by its own id: `(*gitrepo.Repo).ForgetRerere(id)`
(a hex id only; `rr-cache/<id>` is renamed into a fresh directory, then
removed) through `RunGit`, then `RemoveRerereEntry` in one `Store.Tx`,
under `context.WithoutCancel`, and once that has committed `ForgetRerere`
again for each id, so a rebuild that learned the resolution in between
leaves neither the resolution nor, since it records its entry after it,
an entry without one (`Deps.BeforeRerereForgetRecord` is the test hook
before the transaction). `patchstatus.go` adds `GET
/workspaces/:slug/patch-status`, a read route with the scope
`workspaces:read`: the record from `Guard`, `IntegrationBranch` and its
head through `ResolveBranch` in the copy, the two newest rebuilds through
`Jobs.List` (limit 1, any status, then `completed`), `ListLivePatches`,
each patch's `last_rebuild_result` from the job history read newest first
in pages of 100 until every patch has one, and `CountRerereEntries`.
`recovery.go`'s `DiscardRebuildLeftovers` calls `DiscardScratches`
through `RunGit` for the copy of every carry-patch workspace whose clone
work is not outstanding, logging a copy it cannot open or clean with
`workspace` and going on; `composeCarryPatch` registers it as the start
hook `carry_patch_recovery`, right after `carry_patch`, so it runs after
the queue's `Recover` and before the job workers start. `purge.go`'s
`PurgeDeletedPatches(ctx, now)` selects, over every workspace, the patches
`deleted` more than `PurgeAge` (7 days) before `now`, and for each opens
one `Store.Tx`: a `DELETE` conditional on the patch being still deleted
and still due, then, when a row went and no patch that is not deleted uses
the branch, `DeleteRef` of `refs/parley/replaced/<branch>` through
`RunGit` (a missing copy or ref is no failure), its value read first; a
failure rolls back, sets a deleted ref back to that value
(`CompareAndSwapRef` from missing), is logged with `workspace` and
`patch_id`, and makes the pass return an error once every due patch was
tried. Its git calls run under `purgeRefLimit` (30 s), since every hub
write waits on that transaction. `Deps.BeforePurgePatch` is its test hook
between the selection and each removal, `Deps.AfterPurgeRef` one after
the ref's deletion inside the transaction. `composeCarryPatch` registers it
as the maintenance task `patch_purge`, with the runtime's clock. An
attempt's set-up holds each git operation of its upstream fetch to
`RebuildGitDeadline` as well as to the whole fetch's deadline
(`limitedUpstream`); `Deps.BeforeRebuildGit` is a test hook at the start
of each git operation of an attempt.

The publish reads the integration branch's head, then opens one
`Store.Tx`: the patch-list changes and their events are written, then the
workspace is checked through the transaction and the compare-and-swap
against the head read made; an error, `gitrepo.ErrRefMoved` included,
rolls the transaction back. The job's heads are stored by `SetResult`
right after the commit, because `SetResult` opens a transaction of its
own. The push uses `gitrepo.Git.Push` with `Force` and an explicit
`Lease` (`OriginHead`, or `""`), under `DefaultRebuildPushDeadline`
(120 s), and `SetOriginHead` after it; `parley.rebuild.push_failed` is
written in a transaction of its own. The follow-up is queued through
`Rebuilds().EnqueueRebuild` with `FollowUpOf` while the job is still
`running`. See [api.md](api.md#what-an-attempt-does) for what each step
answers; the divergences from the package are in
[the erratum](errata/34_rebuild_execution.md).

### Forge reads

`internal/forge` is the hub's client of the forge a workspace's remote lives
on. The hub part `composeForge` (`internal/hub/forge.go`) creates the
`forge.Component` at composition, registers every route its `Routes`
returns, and binds it in its start hook to the workspace records
(`workspace.BySlug`), the settings registry, the secret component (package
39's `Get`, so every token read is registered with the scrubber), the access
rule and the client's options, the hub's environment among them; tests
replace any of these with `(*Composition).AdjustForge`, the client's
transport included. The component stores nothing, so it registers no
remover and no maintenance task.

**Resolution** (`resolve.go`): the parsing and the kind and URL functions
are pure: `ParseRemote` turns a remote URL into
`Repository{Host, Owner, Name}`, `Kind` decides the forge kind from the API
URL setting and the host, `APIURL` gives the API URL, and `CheckAPIURL`
checks a setting again at use with the settings route's own rule,
`workspace.ValidateForgeURL` (the stored value may predate that
validation). `ResolveCredential` is not pure: it reads the credential kind
and token from the workspace's secrets on each call, in the order of the origin
credential of package 10 and the upstream credential of package 31. It
reads the secrets itself rather than calling
`workspace.ResolveOriginCredential` or `carrypatch.ResolveUpstreamCredential`,
because those give a git credential without its kind, which the diagnostic
route reports. `Resolve` joins them and refuses with `forge_api_url_invalid`,
then `forge_unknown`, before any secret is read.

**The client** (`client.go`) is built once by `Bind`. `ClientOptions` holds
the transport, the clock, the sleep, the deadline (30 s), the body limit
(8,388,608 bytes), the version and the environment. The proxy variables are
read from that environment when the client is built, through
`golang.org/x/net/http/httpproxy`, never from the process
([ADR 06](adr/06-load-the-secret-keys-before-the-stores-open.md)). `Do` sends
one request on a context of its own, bounded by the deadline, which its
caller's leaving does not cancel, so that a write can finish; it follows a
redirect only within the request's scheme, host and port, at most 10 times,
waits out and retries once a rate limit whose stated wait fits the time
left, and returns every failure as a `*forge.Failure` of one `FailureKind`
(`redirect`, `timeout`, `rate_limited`, `status`, `unreadable`,
`unreachable`). `MapError` (`errors.go`) maps one to the hub's status,
`error_type` and scrubbed message, without writing a response, for the
route helper, the issue thread's `comments_error` and later callers.

**The route helper.** `(*Component).Route(forge.Route)` builds a
`server.Route` with the PAT scope and the access need, so the pipeline's
scope check and access check run first. Its handler then runs, in order:
for a task fork, the route's need on the parent through the access rule's
`Decide`, which applies no binding, after which the parent's record,
settings and secrets are used; `target` (left out by `NoTarget`, which the
diagnostic route sets); the route's `Params`; `Resolve`; the route's
`Check`, a hook for refusals that need the resolution (the check run
list refuses a cursor whose anchor is not a commit id on GitHub, or a
pipeline id on GitLab, since the anchor goes into the forge request's path,
and writes will add theirs); and `Serve`, given a `forge.Call`
with the resolution and the client. No step reads the workspace's status,
so reads are served in every state. A failure that `Check` or `Serve`
returns is mapped with `MapError`, with `Retry-After` on a `429`, and logged
at debug level with its scrubbed message.

**Forge paging** (`page.go`) builds on `server.ReadPage`. A cursor's position
is the forge's next page, the page size and, where a listing needs one, an
anchor (the head commit or pipeline of the check runs) and a phase (GitLab's
reviews: approvals, then notes in pages of the room the approvals left).
`next_cursor` comes from the forge's `Link` `rel="next"`, so the routes
write it with `server.WriteList` and `(Page).Cursor`, and the issue thread
mints the comments route's cursor with `server.CursorFor`.

**The diagnostic route** builds its document from the resolution's parts
and calls no forge. `webhook.last_delivery_at` comes from a provider that
the webhook ingress registers with `SetLastDeliveryProvider`; with none
registered it is `null`.

**The client side.** `parley forge`, `parley issue` and `parley pr`
(`internal/cli/forge.go`, `issue.go`, `pr.go`) are built on the read command
machinery of `parley scm`; they check `<number>` with
`internal/forge/number`, a leaf package, so the client does not import the
hub's forge component.

### Forge writes

The nine write routes are part of the same component: `(*Component).Routes`
appends `writeRoutes` (`internal/forge/write.go`), so `composeForge`
registers them with the reads. Each is a `writeRoute` built by
`(*Component).writeRoute` on the route helper, with the scope `forge:write`
and the need write, a body limit of 16 MiB and, for the five routes that
create something, `server.Route.Create`, so the platform's idempotency stage
replays a retry before the handler runs. The helper's two hooks carry the
write order: `Params` runs the route's `before` (path parameters and body
rules that do not depend on the forge) and then refuses an archived
workspace or, through a task fork, an archived parent; `Check`, after
`Resolve`, measures the route's texts (a route's input implements
`measured`; `limitTexts` counts code points against 65,536 on GitHub and
1,000,000 on GitLab), runs the route's `after` (rules of the forge kind,
such as GitLab's assignees) and refuses a target whose credential kind is
`none`. Nothing before `Serve` sends a forge request.

The body helpers (`readObject`, `readObjectOrNone`, `readNames` and the
field rules on the decoded members) treat `null` as absent and name the
field in every refusal. A route's `serve` makes its forge requests through
`Call.send`, `Call.sendJSON` or `Call.Get`, which count the requests in
`Call.sent`, through a `writer` of the forge kind (`github` or `gitlab`, the
same types as the reads' `api`). It returns a `written` value, the status,
body and `writeEvent` of the answer, also next to an error, rather than
writing the response. `serveWrite` runs it on
`context.WithoutCancel(r.Context())`, so a write and its event finish when
the caller leaves; when at least one request was sent it records one event
through `Deps.Events.EmitNow` (the event component, set by `composeForge`
from `(*Composition).Events()`), then answers. A failed forge call is mapped
there once with `MapError`, and its `error_type` goes into the event and the
answer, which the route helper's own mapping passes on unchanged. An event
that cannot be recorded is answered `500` `internal`.

The answers reuse the read mappings: an issue through `githubIssue` and
`gitlabIssue`, a comment through `githubComment.comment()` and
`gitlabNote.comment(page)` (a merge-request note's URL is built on the merge
request's page), a pull request through `githubPull.pull()` and
`gitlabMergeRequest.pull()`, and a label through the `Label` type and
`forgeLabel`. The component stores nothing, so it registers no remover.

The commands (`internal/cli/issue.go`, `label.go`, `pr.go`) are declared
through `forgeWrite` (`internal/cli/forge.go`), which adds `<slug>`,
`<number>` with the rule of `internal/forge/number`, `--target` and the
`--body`/`--body-file` input of `internal/cli/textinput.go`; every usage
error is raised while the command line is checked, before any request.

### Forge webhooks

The webhook secret's routes and the delivery list are plain workspace
routes of the same component, appended to `(*Component).Routes` by
`webhookRoutes` (`internal/forge/webhook.go`) and `deliveryRoutes`
(`internal/forge/delivery_log.go`), with the scope `forge:write` or
`forge:read` and the access need write or read. They are not built with the
route helper, which would switch a task fork to its parent: these routes
refuse a fork (`fork_has_no_origin`), read from `access.DecisionOf`. The
`PUT` and the `DELETE` change the secret and record their event in one
`(*store.Store).Tx`, through three dependencies the start hook `forge` adds
to `forge.Deps`: `Store` (`rt.Store`), `SecretWriter` (the secret
component's `Put`, `Delete` and `Available`) and `Log` (the event
component's transactional `Emit`), so that an event that cannot be written
rolls the secret back. The webhook URL is one helper,
`(*Component).webhookURL`, which the diagnostic route uses too.

`composeForge` mounts the ingress with `c.Handle(forge.IngressPattern,
comp.Ingress())`, `POST /hooks/forge/{slug}` on the server's mux outside
`/api/v1`: the REST pipeline's authentication, rate limit, body limit and
content-type check do not apply, and the handler does its own checks in a
fixed order (`ingest`, `internal/forge/ingress.go`): the workspace record
(`Deps.Workspaces`) and the secret (`Deps.Secrets.Get`, on every request
and for every slug, an unknown one and a fork's included, so that the
three `404` refusals cost the same reads),
the scheme and a GitLab token (`subtle.ConstantTimeCompare` of the SHA-256 digests, so the secret's length does not show), the body read
through a 1 MiB + 1 `io.LimitReader`, a GitHub signature (`hmac.Equal`), the
delivery id and event name, then `(*Component).RecordDelivery`. Its errors
are written with `server.WriteError`.

`RecordDelivery` (`internal/forge/delivery.go`) derives the origin and, for
a carry-patch workspace, the upstream repository with `ParseRemote`, runs
`Reduce` (`internal/forge/reduce.go`), a pure function of the forge kind,
the event name, the body and the repositories, and stores the result in one
`Store.Tx` run on `context.WithoutCancel`, so a forge that leaves early does
not cancel it, bounded by 30 seconds: it reads the workspace's row again
(a workspace deleted since the ingress read its record is answered `404`,
so a slug registered again starts with an empty log), then
`INSERT … ON CONFLICT (workspace, delivery_id) DO NOTHING`
into `forge_deliveries` (schema step `forge.DeliverySchemaStep`), where no
inserted row is a duplicate that writes nothing else, then `Emit` of each
relayed event in order. Concurrent copies of one delivery are settled by the
unique key and the store's single writer, not by a lock in the process.
`Reduce` decodes one JSON object, reads the action and the repository, and
asks the forge's mapping (`mapGitHub` in `github_hook.go`, `mapGitLab` in
`gitlab_hook.go`, chosen by `mappingOf`) for the event drafts, which the
builders of `hook_event.go` fill with the metadata of their kind; nothing
else of the payload reaches a draft. `HookEvent.input` adds `forge`,
`target`, `delivery_id` and `actor` and the actor type `forge`.

`forge_deliveries` holds `seq` (the recording order), `workspace`,
`delivery_id`, `forge`, `event`, `action`, `target` (null when no repository
matched), `outcome`, `relayed` (a JSON array) and `received_at` (the text
`timefmt.Format` writes, so its order is time order), unique on
`(workspace, delivery_id)`, and no payload or secret. `composeForge` also
adds the maintenance task `forge_delivery_pruning`, which calls
`forge.PruneDeliveries` with the pass's time minus `forge.DeliveryRetention`
(7 days) and returns the store's error to the loop; registers
`forge.RemoveDeliveries` as a remover of a workspace's owned data
([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md));
and registers `forge.LastDeliveryAt` as the diagnostic route's
`LastDeliveryProvider` with `SetLastDeliveryProvider`.

The commands `parley forge webhook set|unset` and `parley forge deliveries`
are in `internal/cli/forge.go`; `webhook set` has its own run step, which
writes the show-once warning only when the hub's answer holds `secret`.

### Hub events

`internal/hub/events.go` adds the part `composeEvents`, after `composeAccess`
in `parts`. It creates a zero `event.Component`, adds the schema step
`event.SchemaStep` (`hub_events`) and binds the component in the start hook
`events` to `event.Deps{Store: rt.Store, Clock: event.SystemClock(rt.Now),
Scrubber: rt.Scrubber, Parents: ..., Access: c.access, Memberships: ...,
Shutdown: c.ShuttingDown(), Authenticate: c.identity.Authenticate,
MaxStreams: ..., Retention: ...}`, the fork-parent lookup being
`access.ForkParent` read in the emitting transaction, `c.access` the
`*access.Service` that `composeAccess` creates and keeps on the composition,
the memberships the same `identity.ActiveOrgIDs` source that service is bound
to, `Shutdown` the hub's shutdown notification, which releases the
requests the component holds, `Authenticate` the identity service's
authenticator, with which a stream checks its credential again at every
heartbeat, and `MaxStreams` and `Retention` the `[events]` settings
`event.SettingsFrom(rt.Config)` reads. The part registers the keys of
`[events]` with `event.Register` (`internal/event/config.go`) when it is
composed: `retention`, a string read with `timefmt.ParseDuration`, at least
`1h`, default `30d`, and `max_sse_connections`, an integer of at least 1,
default 500; a value that breaks its rule fails the load with an error that
names the key, so the hub exits with status 1 before it listens.
`(*Component).Retention()` returns the loaded retention for the retention
run. The part registers the routes of
`(*Component).Routes()` with `AddRoute` when it is composed. `Bind` reads
`last_seq`, and its error fails the start. The start hooks that run after it,
those of the parts listed after `composeEvents` and of `Options.Compose`, may
emit before the hub listens; a call made before the component is bound
returns an error. The part also registers the background work `events`
(grace one second), which runs `(*Component).Run` until the stop, so the
stop waits for it before the stores close
([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)).

The step creates three tables in the transactional store:

- `hub_events`, one row per committed event, keyed by `seq`, with the
  record's fields, `metadata` as JSON text and `parent_slug`, the parent of a
  task fork, which no consumer sees; indexes on `(workspace, seq)` and on
  `time`, which is stored as the text `timefmt.Format` writes;
- `hub_event_seq`, one row with `last_seq`, the last `seq` assigned, starting
  at 0;
- `hub_event_log_bound`, one row with `oldest_seq`, the lowest `seq` the log
  addresses, starting at 1.

`(*Component).Emit(ctx, tx, in)` stores one event inside `tx`, the caller's
transaction of `(*store.Store).Tx`, so the event commits or rolls back with
the state change it reports. It validates `in` first and writes nothing on a
refusal, then increments `hub_event_seq` inside `tx` (the store runs one
write transaction at a time, so commit order is `seq` order; a rolled-back
transaction gives its number back, and deleting event rows never lowers the
counter, across restarts too), takes a version 4 UUID as `id` (`uuid.NewV4`),
the clock's time in UTC at whole seconds as `time` and the last segment of the
type as `action`, and returns the stored `event.Record`. When `in.Parent` is
empty and the workspace is a task fork, the parent's slug is read from the
workspace record and stored with the event; a caller that deletes the fork's
record before it emits passes `Parent` itself. `(*Component).EmitNow(ctx,
in)` runs `Emit` in a transaction of its own, for what is not a store change.

An `event.Input` is refused with an error wrapping `event.ErrInvalidEvent`
whose text names the field (`invalid event: <field>: <reason>`) when: `type`
does not begin with `parley.`, has fewer than three dot-separated segments or
an empty one; `workspace` is an empty string; `resource_type` or
`resource_id` is empty; `actor_type` is not one of `admin_token`, `api_key`,
`pat`, `forge` and `system`; `actor_id` is missing or empty for `api_key` and
`pat`, or present for the other three; `session_id` is not a UUID in the
8-4-4-4-12 form (it is stored in lower case); or `metadata` is not a JSON
object (`Metadata` is anything `encoding/json` encodes, nil and JSON null
being `{}`). Every failure to write returns an error that is not
`ErrInvalidEvent`; a route that returns it answers `500 internal`.

A `Record` marshals to one line with exactly the members `seq`, `id`, `type`,
`time`, `workspace`, `actor_type`, `actor_id`, `resource_type`,
`resource_id`, `action`, `session_id` and `metadata`, in that order: `null`
for an absent `workspace`, `actor_id` or `session_id`, an object for
`metadata` and `time` as `timefmt.Format` writes it.

Metadata leaves the hub with every event, so `Emit` passes every string value
of it, at any depth (object values and array elements, not member names),
through `Deps.Scrubber`, the hub's scrubber, before the row is written: URL
user information and registered secret values are removed and the string is
cut to 1024 bytes. Member names, numbers (kept as written), booleans and
`null` are written back unchanged, in their order. The record `Emit` returns
holds the scrubbed metadata, the same the log serves.

The component cannot tell which metadata member is a list of paths, so an
emitting package builds one with `event.PathList(meta, key, paths)`: it stores
the first 100 paths, in their order, under `key`, and sets `truncated` to
`true` only when it cut the list. Other lists in metadata are stored whole.

The event log is read by position
([ADR 05](adr/05-deliver-events-by-reading-the-log-by-position.md)).
`(*Component).Bounds(ctx)` returns `event.Bounds{OldestSeq, LastSeq}`, both 0
while no event was ever written; `LastSeq` is the counter, not the highest
stored row, and `OldestSeq` is `hub_event_log_bound`, 1 until something has
expired. `(*Component).ReadAfter(ctx, after, limit)` returns the events
with a higher `seq` than `after`, ascending, at most `limit` (all when
`limit` <= 0), and the position examined: the last record's `seq` when the
limit was reached, else `last_seq`. When `after` is below `oldest_seq` minus
1, at least one event after it has left the log, and it returns no record
but an `*event.PositionExpiredError` carrying `OldestSeq`, which matches
`event.ErrPositionExpired`, so a caller never misses events without knowing.
The routes read the log through the same function, which also returns the
bounds of its snapshot. The bounds and the
records are read in one read-only transaction, which begins without the write
lock and sees one snapshot. Since commit order is `seq` order, every read is a
gap-free run of the log, also while other transactions emit.

`(*Component).Expire(ctx, cutoff)` makes the events older than `cutoff`
leave the log: in a write transaction it sets `oldest_seq` to the lowest
`seq` whose `time` is not older than `cutoff`, or to `last_seq` when every
event is older, so the newest event stays. It never lowers the bound, deletes
no row, does nothing on an empty log (returning 0), and returns the new
`oldest_seq`. Its schedule is the retention run's.

`(*Component).Run(ctx)` follows the commits. The store has no after-commit
hook, so while anything waits it reads `last_seq` every 100 ms of real time,
and at once when `Emit` or `EmitNow` kicks it. While nothing waits, it reads
nothing. When `last_seq` has risen it counts the new events by type, calls the
callbacks registered with `(*Component).OnCommit(fn)` with each new record,
in `seq` order, from its one goroutine, and wakes the waiters. A waiter (the
stream and the long-poll) takes a channel from the unexported
`waitAfter(position)`. The channel is closed once a commit of a higher `seq`
has been observed, and in any case within a second. The waiter then reads the
log by position again, so a missed wake-up delays an event and never loses it.
When its context ends, `Run` wakes every waiter and returns.

`(*Component).Counters()` returns `event.Counters{LastSeq, OpenStreams,
WrittenByType}`. It first catches up with the commits itself, so the answer
is exact. `WrittenByType` counts the events committed since the bind, by
type; a rolled-back emit is never counted. `OpenStreams` counts the stream
places taken with the unexported `takePlace(limit)`, which refuses once
`limit` are taken, and not yet given back with `givePlace()`; the limit of
the stream is `Deps.MaxStreams`, none when it is 0.

`internal/event/visibility.go` decides who may see an event, at the time it
is delivered. `(*Component).Visible(ctx, caller, rec)` returns `(bool,
error)` for an `event.Caller{Admin, UserID, CredentialID, Workspace}`, where
`Workspace` is the binding of a workspace-bound PAT and `""` otherwise;
`event.CallerOf(r)` builds it from `server.CallerOf` and
`identity.PrincipalOf` (the zero `Caller`, who sees nothing, on a public
route). The rules:

- the admin token sees every event, also one of a workspace that no longer
  exists; a caller with no user sees none;
- an event of a workspace is seen by a user whose level, from the readable
  set of `Deps.Access` (`access.Service.Readable`), is `admin`, `owner` or
  `member` on the workspace or on the fork parent stored with the event, so a
  fork's events stay visible to the parent's readers after the fork's record
  is deleted. A slug with no record gives no level. The types beginning with
  `parley.secret.`, `parley.token.` or `parley.webhook.` need `owner` or
  `admin` there (`parley.forge.webhook.set` is not one of them);
- an event of no workspace is seen by the user whose id is
  `metadata.scope_id` when `metadata.scope` is `user`, by the members of the
  active org `metadata.scope_id` names when it is `org` (asked through
  `Deps.Memberships`, the access rule's own membership question, so a blocked
  org grants nothing), and by nobody else when there is no other scope;
- a workspace-bound PAT never sees an event of no workspace. It sees an event
  of a workspace its binding covers, asked through the `event.Coverage`
  installed with `(*Component).SetCoverage`, under its user's level. A token
  bound to a task fork also sees, under its user's level, the events of the
  fork's parent whose `resource_type` is `change` and whose change, numbered
  `resource_id`, has the fork as its source, asked through the
  `event.ChangeSources` installed with `(*Component).SetChangeSources`.
  Without a covered set a bound token sees nothing; without a change lookup
  it sees none of the parent's events. The hub installs the covered set of
  `access.Service` (see [Workspace access](#workspace-access)) and the
  change component's change lookup, which reads the change's source
  workspace (`ReadChange`; a change that does not exist has none).

A read that fails returns `false` with the error, never a grant. The
covered set and the change lookup are kept apart from `Deps`, so the bind in
the start hook does not reset what a part installed when it was composed. A
batch of events (a page of the log, a stream delivery) is decided by one
unexported viewer, which reads the readable set and the active orgs at most
once each, when an event first needs them, asks the coverage once per
distinct slug and the change sources once per distinct change of the batch,
and is dropped with the batch, so a change of access takes effect at
the next delivery. `(*Component).VisibleToOwnerOf(ctx, workspace, rec)`, the
form event webhooks use, is true exactly for the events whose workspace, or
stored fork parent, is `workspace`, of every type, and reads nothing.

The component's `event.Clock` has `Now()` and `NewTimer(d)`, so that tests
control the time events are stamped with and the timers of the waits;
`event.SystemClock(now)` is the hub's.

`internal/event/poll.go` serves `GET /api/v1/events/log` (see
[the API reference](api.md#get-apiv1eventslog)), declared with the scope
`audit:read` and no access mark. It parses `limit` itself rather than through
`server.ReadPage`, which would read `cursor` as the generic list's opaque
cursor, where here it is another name for `after`. The request parsing the
two event routes share is in `internal/event/filter.go`: the query string,
the filters `workspace`, `types` and `session_id` with their matcher (all
filters together, any one `types` entry), and a resume position. A page is
collected by reading the log after the position in batches of 200, each in
one snapshot with the bounds, so `next_after` and `last_seq` agree, keeping
the events that match and that one viewer decides visible, until `limit`
items or the end of the log. An empty page with `wait` above 0 waits on
`waitAfter`, a timer of the component's clock, the request's context and
`Deps.Shutdown`, and reads the log again after every wake-up; when the timer
fires it reads once more, so `next_after` is the position the log holds then.
The page is written without HTML escaping, so its items are the bytes of
`Record`'s JSON form.

`internal/event/stream.go` serves `GET /api/v1/events` (see
[the API reference](api.md#get-apiv1events)), declared with the scope
`audit:read`, no access mark and the route's own `Cache-Control: no-cache`.
It reads the filters with the same parsing as the log, and the resume
position from the `Last-Event-ID` header, which wins, or from `after`;
without either it starts at `last_seq` at open. It takes a stream place
(`takePlace(Deps.MaxStreams)`) before it answers, and when none is free
answers `503 unavailable` with `Retry-After: 5`; it gives the place back
when the stream ends, for any reason. It answers `200` with
`Content-Type: text/event-stream` and starts a writer goroutine. Between the
handler and the writer is the stream's outbox: the handler queues frames,
the writer writes them in order and flushes whenever the outbox is empty, so
a client that reads slowly holds up neither the heartbeats nor the checks.
The handler queues a heartbeat at once and then runs one loop: it reads the
log after the stream's position in batches of 200 through the same snapshot
read as the log, queues each event that matches the filters and that one
viewer per batch decides visible as a `hub_event` frame, and moves the
position to the position examined, also past the events it did not send.
Replay and live delivery are this same loop, so there is no gap and no
repeat between them. When the log has nothing further it waits on
`waitAfter`, one timer on the component's clock, the request's context,
`Deps.Shutdown` and the writer's failure. The timer is due at the next
heartbeat, 30 seconds after the previous one, or earlier when the write
under way reaches 60 seconds before that. At a heartbeat the handler first
authenticates the original request again with `Deps.Authenticate`; a
credential that no longer authenticates ends the stream before any further
frame is queued. It then reads the log once more and queues the heartbeat,
whose `seq` is the position examined.

The stream ends when the client goes away, the hub begins to stop, the
credential no longer authenticates, an event is due while 256 events wait in
the outbox (the event is not queued, so nothing is dropped from the open
stream), or the write under way, which the writer stamps with the
component's clock, has not completed for 60 seconds. On every end the handler
stops the writer and waits for it, so nothing writes to the response after
the handler returns; a write that is blocked on a client that does not read
is ended first with a write deadline in the past through
`http.ResponseController`, so a stop never waits for a client. The one
exception is a response that cannot take a write deadline while a write is
under way: the handler then returns without waiting, rather than wait on the
client, and the writer ends after that write without writing another frame.
A read that fails after the answer began ends the stream too, and the server
logs the error. So does a read that finds the stream's position below
`oldest_seq` minus 1, because events after it left the log since the
previous read: the stream ends without another frame and without a log
line, rather than skip them, and the client that resumes with the last `id`
it received is refused `410` `cursor_expired`.

### Secrets

`internal/hub/secrets.go` adds the part `composeSecrets`, after
`composeEvents` in `parts`. It creates a zero `secret.Component`, registers
`[secrets].key_file` (a string, default `""`), and adds the schema step
`secret.SchemaStep` (`secrets`). Its configuration check calls
`secret.LoadKeys(c.env, path)` on the hub's environment and the path of
`[secrets].key_file`, a relative path taken from `cfg.Dir` as the
`[sandbox]` section's path keys are; an error stops the start as the one
`cannot start` line, before any store is opened, also for the bootstrap, and
stops `backup`, which loads the configuration the same way, as its one
`backup failed` line
([ADR 06](adr/06-load-the-secret-keys-before-the-stores-open.md)). The start
hook `secrets` binds the component to `secret.Deps{Store: rt.Store, Keys:
<the loaded keys>, Now: rt.Now, Scrubber: rt.Scrubber, Events: c.events}`
and reads `(*secret.Component).StartReport`: whether a current and a
previous key are configured, and the number of rows of `secrets` whose
`key_id` is neither key's `secret.KeyID`, counted in SQL without decrypting
anything. A failing count fails the hook. The part's start report
(`addStartReport`) writes it with `(secret.StartReport).Log` once the
listener is bound: the info line `secret encryption` with `key_configured`,
`previous_key_present` and `undecryptable_secrets`, and without a current key
the warn line `secrets cannot be stored: no encryption key configured`. Key
material is never logged: `secret.Key` logs, prints and encodes as JSON as
`[key]`, also inside a struct such as `secret.Keys` that slog's JSON handler
encodes, and the report and the loading errors carry booleans, counts and the names of the
variable or the file only.

The part registers the routes of `(*secret.Component).Routes` and adds
`(*secret.Component).ScopeCheck` to the access checks, after identity's admin
check and access's subset and workspace checks. Every secret route carries
the mark `secret.MarkScope` (`secret.scope`: `user`, `org` or `workspace`),
and the workspace routes also `access.MarkNeed` `owner`, so the workspace
access check answers the member's `403` and the stranger's `404` before the
scope is resolved. The scope check resolves the scope with one function per
scope (`internal/secret/scope.go`): the caller's user id for `user` (the
admin token is refused `403 forbidden`); for `org` the org of
`identity.OrgBySelector`, which the admin token reaches in any status and a
user only when it is among `identity.ActiveOrgIDs`, every other case one
constant `404 org not found`; for `workspace` the path's slug once the
workspace access check has attached its decision. It attaches the scope
with `server.Attach` ([ADR 04](adr/04-let-a-check-attach-a-value-for-the-handler.md)),
and the handlers read it. The list handler pages
`(*secret.Component).ListPage` with `server.ReadPage` and `server.WritePage`,
the lowered key as the position, and reads no value. The create handler
checks the body (`internal/secret/validate.go`), answers
`secret.UnavailableError` without a current key, and then, in one
`store.Tx`, calls `(*secret.Component).CreateEntries` and writes one
`parley.secret.create` event per entry through the component's
`secret.Emitter` (`*event.Component`), so the entries and their events
commit together or not at all. The update handler checks the body's `value`,
answers `secret.UnavailableError` without a current key, and then, in one
`store.Tx`, calls `(*secret.Component).UpdateEntry`, which seals the new
value without opening the old one, and writes `parley.secret.update`; the
delete handler needs no key and calls `(*secret.Component).DeleteEntry`,
which never opens the value, and writes `parley.secret.delete`. Both events
name the key as it is stored, whatever casing the path used.

The part also registers the removal of the secrets of a user and of an org
on the identity service, with `c.identity.RegisterUserRemover` and
`c.identity.RegisterOrgRemover`
([ADR 03](adr/03-register-removers-of-owned-data-on-the-identity-service.md)):
each remover calls `(*secret.Component).RemoveScope` with `secret.ScopeUser`
or `secret.ScopeOrg` and the id the delete gives it. `DELETE /users/:id` and
`DELETE /orgs/:id` call them in their own transaction, before the record is
deleted, so the secrets go with the user or the org; the user delete's
`guardLastAdmin` refuses the only active admin with `409 conflict` before
any remover runs, and a delete that is refused or rolls back keeps them. The
removal reads no value and writes no `parley.secret.*` event.

The part registers the maintenance task `secret_reencryption`, which calls
`(*secret.Component).Reencrypt(ctx, rt.Logger)` (`internal/secret/reencrypt.go`)
in every pass. With a current and a previous key configured, and the two
different, it reads the rows of `secrets` whose `key_id` is the previous
key's `secret.KeyID`, 100 at a time in the order of their `id`, opens each
under the previous key with the binding of its entry and seals it under the
current key with the same binding. It writes each row in a `store.Tx` of its
own with `UPDATE … WHERE id = ? AND key_id = ? AND sealed = ?`, the key id
and sealed bytes it read, so that a row a request updated or deleted in the
meantime keeps the request's result; it never changes `key`, `created_at` or
`updated_at` and writes no event. A value that fails to open stays as it is
and counts as remaining. Without a current or a previous key it converts
nothing. Then it runs the converters other parts registered with
`(*secret.Component).RegisterConverter(fn secret.Converter)`, in
registration order: a `Converter` (`func(ctx, keys secret.Keys) (converted,
remaining int, err error)`) re-seals the values a part keeps outside the
secret table, such as those it stored with `SealValue`, and nothing
registers one yet. The task ends with the info line `secret re-encryption`
with `converted` (the rows it re-sealed plus the converters' counts) and
`remaining` (the rows whose `key_id` is not the current key's, every row
without a current key, plus the converters' counts). A store failure stops
it before the converters and the line and returns the error, which the
maintenance loop logs as `maintenance task failed`; the rows written until
then stay converted, and every value still opens with the two keys. A
converter's error is returned after the line.

### Variables

Variables are a second collection of the same `secret.Component`
(`internal/secret/var_*.go`, `chain.go`, `resolve_vars.go`,
`resolve_secrets.go`), so that a variable route resolves its scope and
answers its refusals exactly as a secret route does. `composeSecrets` adds the
schema step `secret.VarSchemaStep` (`variables`) after `secret.SchemaStep`: a
row holds the key as given, the key with `A` to `Z` lowered (unique per scope
and scope id, which a concurrent create cannot get past), the value in the
clear, and its times in UnixNano, whole seconds. The part registers
`(*secret.Component).VarRoutes` beside `Routes`. Each variable route carries
`secret.MarkScope`, so `ScopeCheck` resolves it, and on a workspace
`access.MarkNeed` `read` for the list and the resolved variables and `write`
for a change, so a member reads and the owner and the admin token change. The
handlers reuse the secret routes' body checks (`decodeValue`, `CheckKey`,
`CheckValue`, and `decodeVarEntries`, which checks every entry's key before
any entry's value where a secret create goes entry by entry), error mapping and event writing
(`eventInput`, `emit`, with `resource_type` `variable`): `CreateVars` counts,
checks for a key present and inserts in one `store.Tx` with one
`parley.variable.create` per entry; `UpdateVar` and `DeleteVar` write
`parley.variable.update` and `parley.variable.delete`. No variable route
needs an encryption key. `PATCH` and `DELETE` on the literal path
`/workspaces/:slug/vars/resolved` are public routes whose handler answers
`405` with `Allow: GET`: the router (`internal/server/route.go`, `match`)
ranks only the routes of the request's method, so without them such a
request would reach the `:key` routes with the key `resolved`.

The resolution chain is `(*secret.Component).Chain(ctx, slug)`: from the
workspace record (`Deps.Records`, `access.StoreRecords`) the steps, highest
priority first, each a `secret.Step{Origin, Scope, ScopeID}`: the
workspace's own scope (`OriginWorkspace`); for a fork whose parent record
exists, the parent's (`OriginParent`); the org scope of the workspace's org
(a fork's from its parent's record, or its own `OrgID` when that record is
gone) while the owner is in `Deps.Memberships` (the active org ids of
`identity.ActiveOrgIDs`) (`OriginOrg`); the owner's user scope while
`Deps.UserExists` says the user exists (`OriginUser`). It takes no caller and
keeps nothing between calls; a slug with no record is
`secret.ErrWorkspaceNotFound`. `ResolveVars` reads every step's variables in
one read-only transaction and keeps, for each lowered key, the whole entry
of the first step with its origin; the resolved route pages it.
`ResolveSecrets(ctx, slug, names)` answers the secrets a sandbox is given
(its callers are the sandbox packages): for each distinct name, the value of
the first step whose secret of that name `Get` reads, with its origin, and
the steps searched. It drops the parent step before any read unless
`Deps.CanWrite` (the access service's `CanWrite`) says the fork's owner may
write to the parent, and never reads the parent's scope for the names the
hub itself reads for credentials (`GIT_PAT`, `GIT_USERNAME`,
`GIT_PASSWORD`, `UPSTREAM_GIT_PAT`, `UPSTREAM_GIT_USERNAME`,
`UPSTREAM_GIT_PASSWORD`, `FORGE_WEBHOOK_SECRET`). A value that cannot be
decrypted fails the whole call with an error that names the key and the
scope and no value. `ResolveSecretsIn(ctx, q, slug, names)` is the same
resolution with the workspace records of the chain read through `q`, the
caller's transaction, so that a sandbox created in the transaction of a
fork create resolves the chain of the fork that is not committed yet; the
memberships, the users, the write access and the values are read as
`ResolveSecrets` reads them. The start hook `secrets` binds `Records`,
`Memberships`, `UserExists` and `CanWrite` beside the other `secret.Deps`.

Variables go with their owner and write no event: `composeSecrets`
registers `secret.RemoveVarScope` for the user and the org scope with
`c.identity.RegisterUserRemover` and `RegisterOrgRemover`, after the
secret removers, and `composeWorkspaces` registers
`secret.RemoveWorkspaceVars` on the teardown set beside
`secret.RemoveWorkspace`, which a fork's removal runs too through
`RemoveOwned` ([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)).

### The logger

`hub.NewLogger(w, level, scrubber)` returns a `*slog.Logger` with
`log/slog`'s JSON handler on `w`, one object per line. The levels are
`hub.LevelTrace` (`slog.Level(-8)`, written as `TRACE`), debug, info, warn
and error; lines below the configured level are dropped. Its `ReplaceAttr`,
which the JSON handler calls for the message and every attribute, those of
`With` and of groups included, passes every string value and the text of
every `error` value through `scrubber.Scrub`. Any other value is scrubbed
as its `fmt` text (a `[]byte` as text): when scrubbing changes that text
(it removes something, or cuts a text of more than 1024 bytes), the
scrubbed text is written in the value's place, and otherwise the value keeps
its JSON form. The values of an `http.Header`'s `Authorization` and
`Proxy-Authorization` headers are written as `[redacted]`, whatever their
shape. Keys, `time` and `level` are written as they are. `Run` builds it on `Stderr` before the configuration is
loaded, at info, sets its level once the configuration is loaded, installs it
with `slog.SetDefault`, and hands it to the server as `Options.Logger`.

### Idempotency records

`internal/hub/idempotency.go` adds the schema step `idempotency_records` to
the transactional store: a table `idempotency_records` with `key`, `caller`,
`route` (together the primary key), `request_hash` (the 32-byte SHA-256),
`body`, `header` (the headers as JSON) and `written_at` (nanoseconds since
the Unix epoch). Its `server.IdempotencyStore` finds a record by key, caller
and route, writes one with an upsert in a transaction of `Tx`, and deletes
the records written at or before a cutoff. The server calls `DeleteExpired`
with its clock, the hub's `Now`, so a record written before a stop is
replayed after the next start until it is 24 hours old.

The flags, exit statuses and log lines are described in [cli.md](cli.md),
the configuration in [configuration.md](configuration.md).

### Readiness probes

A readiness probe is a `server.Probe`, a `func(context.Context) error` that
returns an error when one part of the hub is not ready. `GET /readyz` calls
every probe with the request's context, including those after one has failed,
and answers `503` with `{"status":"not ready"}` if any failed. With no probe
registered the hub is ready. A probe's error text never goes into the
response.

## The start-up check

`composeSandbox` (`internal/hub/sandbox.go`) registers the `[sandbox]`
section (`sandbox.Register`), checks it with `(*sandbox.Service).Configure`
as a configuration check, right after the load, and adds the start hook
`sandbox_runtime_check`. With sandboxes off the hook builds no client and
contacts no runtime. With sandboxes on it registers the gateway token and the
OIDC client secret with the hub's scrubber, builds the client with the
runtime factory (`sandbox.RuntimeFactory`, given the settings, the scrubber
and the recorder; the default is `openshell.New`) and calls
`(*sandbox.Service).Start`, which runs `sandbox.RunStartCheck` and keeps the
client and the release once it has succeeded. A failure is the hook's error
and stops the start before the listener is bound.

`RunStartCheck(ctx, settings, client, CheckOptions{ExternalURL, Now, Budget,
Releases, Recorder})` makes, under one deadline `Budget` (30 seconds,
`sandbox.StartCheckBudget`) after it began on the clock `Now`, these calls
in order and stops at the first failure: `Release`, then the release against
the range (`ReleaseRange.Contains`; `SupportedReleases` unless `Releases`
replaces it); `EnsureWorkspace`; `InstallProviderProfile` of `parley-hub`
(`sandbox.HubProviderProfile`) with the variable `PARLEY_API_KEY`, one
endpoint of the scheme, host and port of `[server].external_url` with the
paths `/api/v1/` and `/git/`, and no program; `GetProviderProfile` once per
distinct `provider_profile` of the credentials; the exposed-secret names of
each profile against the variables of the provider profiles of the
credentials it attaches; `ValidatePolicy` with the base document of each
profile that names a `policy` file. A result of kind unavailable, and the
deadline passing, name `sandbox.gateway_url`; a missing provider profile
names `sandbox.credentials[<name>].provider_profile`; a rejected policy, or
a client without the validation (`ErrUnsupported`), names
`sandbox.profiles[<name>].policy`. The error's text is scrubbed of the
gateway token and the OIDC client secret. On success the check sets each
validated profile's `PolicyHosts` to the hosts the runtime reported, sorted,
sets `parley_sandbox_runtime_up` to `1` on the recorder and returns the
release. `(*sandbox.Service).Version()` then gives `GET /version` its
`sandbox_runtime` member, `supported` (`ReleaseRange.String()`,
`>=0.1.0 <0.2.0`) and `connected`; it makes no call, so it does not change
while the runtime is unreachable.

## Sandbox route declarations

The sandbox packages add their routes through the `*sandbox.Service` of
`(*Composition).Sandbox()`, which declares them on top of the pipeline's
positions rather than adding positions of its own:

- `Route(server.Route)` declares a route of the whole hub, such as the
  profile list: it sets `Route.Feature` to the service's feature check and
  wraps the handler with the runtime-error answer, and keeps the route's
  scope and marks.
  `composeSandbox` adds the profile list, `(*sandbox.Service).ProfilesRoute()`,
  this way, with the scope `sandboxes:read` and no workspace need; its
  handler answers from the loaded settings and calls no runtime. It adds
  the hub-wide sandbox list, `(*sandbox.Service).SandboxesRoute()`, the
  same way with no scope: its handler refuses every caller but the admin
  token (`server.Caller.Admin`, not identity's admin rule, which also admits
  the API keys of admin users) with `403 forbidden`, reads the page with
  `server.ReadPage` and the `state` and `user` filters, and asks the
  registered `sandbox.Source` for `limit + 1` items after the cursor's
  position (`created_at`, `id`, both descending) before writing them with
  `server.WritePage`. The source is an interface with the one operation
  `List(ctx, Filter, after *Position, limit)`; the package that stores
  sandboxes registers it with `RegisterSource` while the hub composes or in
  a start hook. Without one the list is empty, and an error of the source is
  `500 internal`, never the runtime-error answer: the list calls no runtime.
- `WorkspaceRoute(kind, server.Route, opts...)` declares a route under
  `/workspaces/:slug/sandboxes`: on top of what `Route` adds it sets
  `Route.Scope` to `sandboxes:read` or `sandboxes:write` and the mark
  `access.MarkNeed` to `read` or `write`, for `sandbox.KindRead` and
  `sandbox.KindWrite`. The doc comment of `Kind` names the kind of each
  route of the sandbox packages. With `sandbox.NeedsUser()` the handler is
  wrapped once more, so that the admin token is answered `403 forbidden`
  with the message `the admin token cannot create a sandbox`.

Routes are declared while the hub composes, before the configuration is
loaded, so the feature check reads whether sandboxes are on when a request
comes and answers `404 sandboxes_disabled` while they are off. It runs in
the pipeline's feature position: after the body checks, before the binding
guard. The needs-a-user rule and the runtime-error answer wrap the handler,
which runs after every access check, so they come after workspace access and
before the handler reads its body. The runtime-error wrapper turns an error
of kind `ErrUnavailable` or `ErrUnsupported` that the handler returns, and
did not turn into another answer, into `sandbox.RuntimeError` with the
error's text passed through the hub's scrubber, which the start hook
`sandbox_runtime_check` gives the service (`SetScrubber`) and which knows
the gateway token and the OIDC client secret; a `*server.Error` and every
other error are left to the pipeline. A handler that answers a runtime error
itself calls `sandbox.RuntimeError` with a message it scrubbed.

The no-widening decision is for the handlers that run a command in a
sandbox, attach to it or write a file into it, so that a weak token cannot
act through a sandbox a stronger credential created. `sandbox.HubAccess`
(`Scopes`, `Paths`, `Paths` nil for no paths limit) is what the sandbox's
hub credential may do, and nil is a sandbox without one.
`sandbox.Covers(identity.Principal, *sandbox.HubAccess)` answers it: every
caller covers nil, the admin token and an API key cover everything, a PAT
covers a hub credential when `identity.Satisfies` passes for each of its
scopes and, when the PAT is limited to paths, the hub credential is limited
too and `identity.FirstUncovered` finds none of its globs outside the PAT's;
a caller of any other type covers nothing. `sandbox.RequireCovers(r, ha)`
reads the caller with `identity.PrincipalOf` and returns `403
insufficient_scope` when it does not cover `ha`; the handler calls it once
it has found the sandbox and before it validates the body, and returns the
error without acting.

## The sandbox runtime client

All of the hub's traffic to the sandbox runtime goes through
`runtime.Client` (`internal/sandbox/runtime`). A client acts in one OpenShell
workspace, the one it was built for, and in no other. Its operations, each
taking a context, are grouped as follows; their names in snake case
(`runtime.OpRelease` = `release`, `runtime.OpGetSandbox` = `get_sandbox`, …)
are the operation names errors and the fake's call list carry:

| Group | Operations |
|---|---|
| Gateway | `Release`, `EnsureWorkspace`, `Reachable` (the flag; no call) |
| Provider profiles | `GetProviderProfile`, `InstallProviderProfile` (create or replace) |
| Providers | `EnsureProvider` (create or replace), `DeleteProvider`, `AttachProvider`, `DetachProvider`, `SandboxProviders`, `ListProviders` (without values) |
| Policy | `ValidatePolicy`, `GetPolicy`, `AddPolicyRule`, `ListProposals`, `ApproveProposal`, `RejectProposal` |
| Sandboxes | `CreateSandbox`, `GetSandbox`, `ListSandboxes`, `DeleteSandbox`, `StopSandbox`, `StartSandbox`, `WaitReady`, `WatchSandboxes`, `RunMain` |
| Exec | `Exec`, `ExecStream`, `ExecInteractive` |
| Files | `Upload`, `MakeDir`, `Stat`, `Download`, `Walk` |
| Logs | `Logs` |

A sandbox is named by its name within the workspace. A policy value
(`runtime.Policy`) has three parts: the base document, the protected rules
and the added rules; a rule (`runtime.Rule`) is a name and a JSON object in
OpenShell's schema that the hub does not interpret. The streams
(`SandboxWatch`, `ExecStream`, `Terminal`, `LogStream`) end with `io.EOF`
when the caller's context ends, and with an error of kind `ErrUnavailable`
when they break.

A failure is a `*runtime.Error` of exactly one kind, which callers test with
`errors.Is`: `ErrNotFound`, `ErrInvalid` (the runtime's reason is kept in
the text), `ErrUnsupported` (the runtime has no such operation; it is never
imitated with other calls) or `ErrUnavailable` (everything else). Its text
reads `sandbox runtime: <op>: <kind>: <reason>`. It keeps text only, not the
error it was made from, so that nothing an implementation scrubbed can be
reached through `errors.Unwrap`. `runtime.KindOf` gives an error's kind, and
takes an error of none of them for `ErrUnavailable`.

Every implementation keeps its reachability flag in a `runtime.Reachability`:
a result of kind `ErrUnavailable` clears it, every other result sets it, and
each change is reported once, in order, to the `runtime.Recorder` as
`parley_sandbox_runtime_up` (`runtime.MetricUp`) `1` or `0`. The flag starts
set. The recorder has the method set of the hub's metrics registry (`Add`,
`Set`, `Observe`, each with a name, a label map and a value, each returning
an error); `runtime.NopRecorder` records nothing.

`runtimetest.New(t, opts...)` returns the in-memory runtime, `Fake`, and
`(*Fake).Client(workspace, recorder)` a client of it. It keeps per workspace
the provider profiles, the providers, the sandboxes with their status, the
policy values, files, log entries and pending proposals. A sandbox's files
are held in memory, or with `WithHostDirs()` in a directory of the host
(`HostDir`) in which `Exec` and `RunMain` run real commands; otherwise
commands are run by a script (`SetExec`), and without one a command exits 0
with no output and a main process runs until `EndMain`. Its terminal echoes
its input. A new fake reports a healthy gateway of release `0.1.8`;
approving a proposal adds its rule to the sandbox's added rules. Its
controls are `SetUnreachable`, `FailOperation` (an operation by name, with a
kind and a text, until cleared with a nil kind), `SetRelease`,
`AddProviderProfile`, `RejectPolicies`, `SetPolicyValidation`, `SetExec`,
`SetState`, `EndMain`, `DropWorkDir`, `EmitOutput`, `EmitPolicyLog`,
`AddProposal` and `BreakStream`; `Calls` returns the calls its clients
received in order, `Streams` the open watches, followed logs, exec streams
and terminals, and `Spec` and `Provider` what a sandbox was created with and
a provider with its value. Its processes and streams end with the test.

`runtimetest.RunContract(t, newBackend)` runs the contract tests against the
`runtimetest.Backend` that `newBackend` returns for every test: a function
that returns a client of the backend for a workspace and a recorder, a
switch that makes the backend unreachable, where the backend can, a way to
reject policy values and to add a proposal, and `EndpointsWithoutPaths`,
which a backend sets when its provider profiles keep no endpoint paths (get
is then expected to return the profile without them). The assertions about
an operation that returns `ErrUnsupported` are skipped, so the contract
holds for a runtime that lacks the operation. Only the policy value test
creates its sandbox in a working directory with a base document; every other
test creates one with neither. `(*Fake).Backend()` gives the fake
in that form.

### The OpenShell implementation

`openshell.New(settings, openshell.Deps{Scrubber, Recorder, Dial})` builds
the client the hub uses. It registers the gateway token and the OIDC client
secret with the scrubber, builds the SDK configuration with
`openshell.BuildConfig(settings)` and the SDK client with `Deps.Dial`
(`v1.NewClient` when nil), which connects on the first call, and acts in
`openshell_workspace`. `openshell.NewWithClient(sdkClient, workspace, deps)`
builds the client over an SDK client the caller has, such as the SDK's
in-memory `fake.NewClient()`; closing that SDK client makes every later
operation that calls it fail as `ErrUnavailable`, while the operations the
SDK lacks keep answering `ErrUnsupported` without calling it.

`BuildConfig` takes the address from `gateway_url`: its host and port (when
it gives none, 443, or 80 for an `http` gateway), with the `http://` scheme kept for a loopback gateway, which is
how the SDK is told to connect without TLS. Mutual TLS gives the SDK the
certificate and key files and no token source; `token_file` gives a static
bearer token; the OIDC keys give the SDK's client-credentials token source,
which obtains a token on the first call and renews it before it expires;
`tls_ca` is the only CA the gateway's certificate is checked against. Over
`http` to a loopback gateway the token is sent without TLS, which the SDK
otherwise refuses.

Every operation passes the workspace to its SDK call. An operation that
does not stream runs with a context that ends after 30 seconds or with the
caller's, whichever is sooner, and the client stops waiting for the SDK when
it ends; `ExecStream`, `ExecInteractive`, `Upload` and `Download` run with
the caller's context. The SDK's errors map to the kinds as follows: its
not-found is `ErrNotFound`; an invalid argument, a resource that exists
already and a conflict are `ErrInvalid` with the gateway's message and the
fields it names; a call that is not implemented, and a file transfer in an
SDK built without its transport, are `ErrUnsupported`; everything else, a
refused connection, an unauthenticated or refused credential, a server error
and a call that did not end in time included, is `ErrUnavailable`. Every
error's text is scrubbed.

The SDK in `go.mod` (v0.1.8) has no call for `ValidatePolicy`, `GetPolicy`,
`WatchSandboxes` (it watches one named sandbox), `RunMain`, `MakeDir`,
`Stat`, `Walk` and `Logs` (its logs are the gateway's and the supervisor's
log lines), nor for `Exec` and `ExecStream` with input on stdin; they answer
`ErrUnsupported` without calling it. Where it has a call but cannot carry a
part of the request, a request with that part is `ErrUnsupported` too and
does not reach the SDK, so that no sandbox runs without the policy or
outside the directory it was asked for: `CreateSandbox` with a working
directory or a base document (the SDK takes typed policy rules: the
protected and added rules go as named network rules, and a rule its network
rule cannot hold is `ErrUnsupported`), and `StartSandbox` with a policy
value or an environment. With SDK v0.1.8 no sandbox can therefore be created
with a base policy or in a working directory; the caller answers
`ErrUnsupported` as its own package says.
`InstallProviderProfile` installs a profile with one credential named after
the profile; the SDK's profile endpoint is a host, a port and a protocol,
which takes the endpoint's scheme, and has no path, so an endpoint's paths
are not sent and the credential is authorized on every path of its host. `EnsureProvider` stores the value under the one credential
of the provider's profile. `AttachProvider` first reads the provider in the
workspace, so a provider of another workspace is not found.
`ListProposals` reads the pending draft chunks of every sandbox of the
workspace; `AddPolicyRule` uses the SDK's merge operation that adds a named
network rule. `Upload` and `Download` go through a temporary file, because
the SDK transfers from and to local paths. A sandbox's state follows its
phase: `Ready` is `running`; `Stopping`, `Stopped`, `Suspending`,
`Suspended`, `Completed`, `Error` and `Deleting` are `stopped`; a sandbox
waiting to restart is `restarting`; the rest is `pending`. The SDK reports
nothing of the working directory, which counts as present while the sandbox
exists.

The contract tests run against this implementation over the test gateway
with what the SDK's in-memory client does not implement added in the
package's tests: commands run as processes of the host, files and policy
drafts kept in memory, and the provider profile `anthropic` the contract's
providers name. The backend sets `EndpointsWithoutPaths`. The assertions
about the operations this implementation answers `ErrUnsupported` are
skipped, and so is the policy value test, whose sandbox has a working
directory and a base document.

`openshelltest.New(opts...)` (`internal/sandbox/runtime/openshell/openshelltest`,
test support) returns a `Gateway`: a `v1.ClientInterface` that stands in for
the gateway in the tests of this implementation, of the hub and end to end.
It runs over `fake.NewClient(opts...)` (`Fake()` returns it), keeps provider
profiles per workspace (`AddProfile` preloads one, `Profile` reads back what
was installed) and can be made unreachable and reachable again
(`SetUnreachable`), every call then failing as unavailable; `Close` closes
the in-memory client for good. `runtimetest.Fake` stays the double of the
runtime that the other sandbox packages test against.

## Stored sandboxes

`internal/sandbox` keeps the hub's record of every sandbox in the table
`sandboxes` (`SchemaStep`, registered by `composeSandbox` whether sandboxes
are on or off), one row per sandbox: every member of the sandbox object but
`runtime_reachable`, and what the other sandbox packages need of it later
and no answer shows: the profile snapshot (`Snapshot`: ttl, idle timeout,
checkpoint, allow_proposals, hub_access, the profile's env, the base policy
document read from the profile's policy file, the credential and
exposed-secret names with each credential's provider profile), so that a
sandbox outlives a change or the removal of its profile; the bodies of the
workspace sandbox rules recorded at creation; the id of the hub
credential's token; the creating user, the org and the family root (the
parent's slug for a fork) the quotas count by; the provisioning marker
(`create` or `start`), the mark that the runtime holds the sandbox and the
mark that the runtime caused the state `provisioning`; and the time it last
became `ready`. It has no foreign key to `workspaces`: a deleted sandbox
outlives its workspace until it is purged. Times are Unix seconds, lists and
objects JSON. Its id is `sbx_` and 32 lowercase hexadecimal characters from
`crypto/rand`.

The start hook `sandbox_records` binds the service (`Bind(sandbox.Deps)`)
to the store, the hub's clock and timers, the event component, the metrics
recorder, the opening of a workspace's repository, the secret resolution
(`ResolveSecretsIn` with a transaction, `ResolveSecrets` without), the
minting component `access.MintBound`, `identity.RevokeTokenByID`,
`[server].external_url`, the org slug and user record lookups, the logger,
the access component's `Readable` and the hub's `ShuttingDown`; tests change
them with `AdjustSandbox`. With sandboxes on it then registers the service
as the fork component's sandbox component. The routes `POST`, `GET` (one)
and `GET` (list) of `/workspaces/:slug/sandboxes` are declared through
`WorkspaceRoute` (create with `NeedsUser`), and `StoredSource` is
registered as the source of `GET /sandboxes`.

**Transitions.** `(*Service).Transition(ctx, tx, id, from, to, reason, ev)`
is the one way a sandbox's state changes, for this package and the ones
built on it: one table of allowed pairs (`ErrInvalidTransition` otherwise,
`ErrNotFound` for an unknown id), a conditional `UPDATE … WHERE id = ? AND
state = ?` that reports whether it applied, `updated_at`, `ended_at` for an
ended target (cleared for the others), and the event, completed with the
sandbox as resource, its workspace and session, in the same transaction
(the caller's, or one of its own). Since the store admits one write
transaction at a time, two racing moves that expect the same state apply
once. An applied move reports `parley_sandboxes` from its transaction's
counts and has a report for once it holds: it observes
`parley_sandbox_seconds` when it leaves `ready` for an ended state and
notifies the reads that wait for a sandbox. `Transition` makes that report
after its own commit, or at once with the caller's transaction;
`(*Service).TransitionTx(ctx, tx, …)` returns it for the caller to call
once its transaction has committed, as the provisioner's ends do, so that a
move that rolls back reports nothing. A waiting read reads the record on a
read connection, never in the store's turn of writes; a notification made
before a caller's commit is caught by its re-read every second. The delete hooks and purge hooks are lists on
the service, run in order in the transaction that marks a record `deleted`
or removes it, stopping at the first error.

**Create.** `(*Service).Create(ctx, tx, CreateInput)` is the one create the
route and the fork component share: with a nil `tx` it runs in a store
transaction of its own and wakes the provisioner after the commit; with the
caller's transaction it reads and writes in it and wakes nothing. It reads
the workspace's record again in that transaction and judges that one, not
the record the caller read. In order: the body (`validate.go`), the session link (the registered
`SessionLinker`; without one every `session_id` is refused), the
workspace's state (`workspace.RequireReady`, and `fork.GuardUpdate` for a
fork's parent), the resolution of the profile's credentials and exposed
secrets (`Resolve`, once per create), the quotas (`CheckQuota`, counted in
the inserting transaction), the hub credential (minted through
`MintBound` in the same transaction, its value never stored and kept in
memory for the provisioner, with the resolved values, from the mint on,
before the commit, since the provisioner may find the sandbox as soon as
it commits; a create that fails or does not commit drops them), the workspace sandbox rules of the
registered `WorkspaceRuleProvider`, the insert and `parley.sandbox.create`.
Errors after the resolution are scrubbed of the resolved values.

**Provisioning.** `RunProvisioner`, the background work
`sandbox_provisioner` (grace 5 seconds), makes a pass when a create wakes
it, every second on the hub's clock, and after the start hook
`sandbox_provisioner` (`ResumeProvisioning`); a pass reads the sandboxes in
`provisioning` with the `create` or the `start` marker (a start's
provisioning is described under [the life of a
sandbox](#the-life-of-a-sandbox)) and works each at most once at a
time, at most 8 at once, each under a bound of 10 minutes on the hub's
clock. A provisioning first deletes what the runtime holds of the sandbox
(the sandboxes and providers labelled `parley.sandbox=<id>` or named after
it, by name when the runtime cannot list them), which a provisioning the
hub stopped in may have left whether or not it marked the sandbox created,
and clears that mark; resolves the secrets again; takes the hub credential
from memory, or, after a restart, revokes the stored token and mints a
replacement (`MintHubCredential`); registers the providers (`<id>-<credential>`,
`<id>-hub`); builds the environment (`environment.go`), leaving out the
variables of the attached providers' profiles and `PARLEY_API_KEY`, whose
placeholders the runtime sets; asks the registered `PolicyComposer` for the
policy (the default: the snapshot's base document with `ProtectedRules`);
creates the sandbox, named by its id, in `/workspace`; waits until it is
ready; checks the repository out over `Exec` (`checkout.go`) and calls the
registered `Restorer` for `restore_from` (an error of the restorer fails the
sandbox with `checkout_failed`, a missing restorer or source record with
`provision_failed`); starts the main process; and moves
the sandbox to `ready` with `parley.sandbox.ready`, storing the checked-out
commit in the same transaction. A failure deletes what the runtime holds of
the sandbox, by label and name as at the start (with a context of its own,
30 seconds), and one transaction moves the sandbox to `failed` with
`parley.sandbox.fail`, clears the marker, clears the mark that the
runtime's sandbox was created once the runtime no longer holds it, and
revokes the hub credential; once it has committed the sandbox's values are
forgotten (`ForgetValues`). A sandbox moved or removed meanwhile is not
touched beyond that mark: its runtime leftovers are deleted, its hub
credential revoked and its values forgotten. The transition's report and
`parley_sandbox_creates_total` follow the commit of the transaction that
ends the provisioning.
At the stop the provisioner's context ends and it writes nothing, so the
marker makes the next start take the sandbox up again. A sandbox created
in a fork transaction that rolled back leaves its hub credential value in
memory until the first pass at least 60 seconds later, which drops it.

**Scrubbing.** `Scrub(values, text)` replaces every exact occurrence of a
sandbox's credential secrets, its exposed secrets of at least 8 bytes and
its hub credential by `[redacted]`, longer values first, and cuts the text
to 1,024 bytes on a UTF-8 boundary; `ScrubSandbox` does it with the values
the service keeps per sandbox (`SetValues`), refilled by resolving the
record's names again when the list is empty, as after a restart.
`ForgetValues` drops them: this package calls it for a create that does
not commit, a provisioning that ends `failed` or finds its sandbox moved or
removed, and a fork-transaction sandbox that was rolled back; the packages
that stop and delete sandboxes call it when they end one.

**Metrics.** Through the recorder `SetSandboxRecorder` gives (none by
default): `parley_sandbox_creates_total{result}` for each provisioning that
ended `ready` or `failed`, counted once the ending transaction has
committed, `parley_sandboxes{state}` for the five states other than
`deleted`, set from the store's counts after every insert, every applied
transition and the removals this package makes (the drop of a sandbox
whose fork transaction rolled back); a caller that removes a record calls
`ReportStates`. And `parley_sandbox_seconds`, the seconds a sandbox was
`ready` when it leaves `ready` for an ended state, observed by the
transition's report.

## Using a sandbox

`(*sandbox.Service).UseRoutes` declares the five routes that use a sandbox
(`internal/sandbox/use.go`), which `composeSandbox` adds: `POST …/exec`,
`PUT …/files`, `GET …/files`, `GET …/logs` and `GET …/attach` under
`/workspaces/:slug/sandboxes/:id`. They come from one table, `useTable`, each
entry with its kind (read or write, through `WorkspaceRoute`), whether the
no-widening rule applies, the states it serves, its body limit (exec: 8 MiB of
JSON) or `StreamBody` (the upload), its validation and its operation. So the
pipeline runs the feature check, the binding guard, the PAT scope and
workspace access, and then the entry's handler, the gate, runs in a fixed
order: the sandbox (`readSandbox`, `404 sandbox_not_found`), `RequireCovers`
with the record's `hub_access` (exec, upload, attach), the validation, the
state (`409 sandbox_not_ready`), and only then the operation, which gets the
caller, the slug and the record and looks none of them up again. An error the
operation returns that is a runtime error becomes `502
sandbox_runtime_error`, its text passed through `ScrubSandbox` and then the
hub's scrubber; any other error keeps its kind with its text scrubbed the same
way for the log. An operation that answers what it can act on (a path the
runtime does not find) returns a `*server.Error` itself. The runtime's
sandbox is named by the sandbox id.

**Exec** (`exec.go`, `exec_stream.go`). One `executor` per service holds the
count of running execs per sandbox in memory: a slot is taken after the gate,
`429 rate_limited` without `Retry-After` when `[sandbox].max_concurrent_execs`
are taken, and released when the exec ends in any way. A command runs through
`ExecStream` with the request's `env` alone (the runtime lays it over the
sandbox's environment), under a context that ends at the timeout (a timer of
the hub's clock, `Deps.NewTimer`), when the caller leaves and when
`Deps.Shutdown` closes; ending it kills the command. The collected form keeps
1 MiB per stream and drains the rest; the streamed form, chosen by an
`Accept` that names `text/event-stream`, writes its headers once the runtime
has started the command, so every refusal before is the JSON envelope, and
flushes each frame of at most 32 KiB.

**Files** (`files.go`). `resolvePath` joins a relative path with `WorkDir` and
cleans it lexically; nothing outside `/workspace` passes. The upload reads
the body itself (the route's `StreamBody`), counted against
`[sandbox].max_transfer_size`, and writes a file with `Upload` or unpacks a
tar stream entry by entry with `MakeDir` and `Upload`. The download asks
`Stat`, then sends a file from `Download` with its `Content-Length` or a tar
stream built from `Walk`; a failure after the headers, the size limit
included, aborts the connection (`http.ErrAbortHandler`).

**Logs** (`logs_store.go`, `logs_collect.go`, `logs_read.go`). The lines live
in the table `sandbox_logs` (`LogSchemaStep`): `seq`, the order of storage,
the sandbox id, `time` (Unix seconds), `source`, `stream` and `line`, and
`action`, `program`, `host`, `port` and `credential` for policy lines; it has
no foreign key to `sandboxes`. `CollectLogs`, the background work
`sandbox_log_collector` (grace 5 seconds, only with sandboxes on), makes a
round at start and every `LogCollectInterval` (5 seconds) on the hub's clock:
it reads the `ready` sandboxes, starts a follower for each that has none and
stops the others'. A follower follows `Logs` twice, for the sources `process`
and `policy`, from the second of the sandbox's last stored line of that
source, skipping the lines of that second already stored; a broken stream
ends the follower and the next round opens it again. Lines are split at line
ends and at `MaxLogLine` (16,384 bytes), redacted with the sandbox's values
(`ValuesOf`) without the 1,024-byte cut, stored, and announced on
`LogLinesStored`. `LogLines` reads them by `LogSelection` (source, since,
tail, after a `seq`, limit). The read answers pages through the list helpers;
the follow reads past the last `seq` it sent when the collector announces
lines, when a sandbox changes state, or every `LogFollowInterval` (1 second)
on the hub's clock, and ends at `deleted`, at the stop or when the caller
leaves. `RetainLogs`, the maintenance task `sandbox_log_retention`, deletes
lines older than `[sandbox].log_retention` and those of a sandbox whose record
is gone; `PurgeLogLines`, a purge hook `composeSandbox` registers, deletes a
sandbox's lines in the transaction that removes its record.

**Attach** (`attach.go`). The validation requires a WebSocket upgrade
(version 13) and reads `cols` and `rows`; the operation opens the terminal
with `ExecInteractive` (`/bin/sh` in `/workspace`) before the upgrade, so a
runtime failure is still the envelope, then upgrades with a
`golang.org/x/net/websocket` server that accepts any origin, through a writer
that hijacks with `http.ResponseController`. Two goroutines relay: frames from
the caller (binary to the terminal, a resize text frame to `Resize`, any
other text frame ignored) and the terminal's output in binary frames. The
caller leaving, the request's end or the stop close that terminal only.

**Activity and observers** (`activity.go`). The gate gives each request an
activity; the operation starts it once it acts (after the exec slot, the
upload's length check, the download's `Stat`, the terminal's opening), and
the gate ends it from a `defer`, so an aborted download ends it too. Start and
end set `last_activity_at` with one conditional statement that only raises it
(`UPDATE … WHERE id = ? AND last_activity_at < ?`); an attach also on a frame
from the caller once a minute, and the collector on each stored process line.
The end adds one to `ExecMetric` (`parley_sandbox_exec_total{result}`) for an
exec, and then calls each `Observer` registered with `RegisterObserver` with
an `Operation`, in registration order, each under `recover`; a failure is
logged and changes nothing. The telemetry part registers the recorder of
sandbox use as one (see [Agent telemetry](#agent-telemetry)).

## The life of a sandbox

`internal/sandbox` also moves a sandbox through the rest of its life once it
is created: one file per operation, each a method of `*sandbox.Service`, and
every state change through `TransitionTx`, conditional on the state read, so
that racing requests and the hub's own work apply one transition and write
one event (an operation whose transition did not apply writes nothing
else). Stop and delete also take turns per sandbox (`Service.lifecycle`, an
in-process lock per id), so racing stops make one runtime call; each
re-reads the record once its turn comes. Once the runtime was asked to
stop a sandbox, the stop records its move on a context of its own, even
when the caller has left.

| File | Operation |
|---|---|
| `checkpoint.go` | `Checkpoint(ctx, rec, actor)`, and `POST …/checkpoint` |
| `stop.go` | `Stop(ctx, rec, reason, actor)` (`api`, `idle`, `archived`), and `POST …/stop` |
| `start.go` | `StartSandbox(ctx, rec, actor)`, and `POST …/start` |
| `restore.go` | `Restore(ctx, rec, source)`, the registered `Restorer` |
| `delete.go` | `Delete(ctx, rec, reason, actor, checkpoint)` (`api`, `expired`, `workspace_deleted`), and `DELETE …/sandboxes/:id` |
| `extend.go` | `Extend`, and `PATCH …/sandboxes/:id` |
| `timers.go` | `IdlePass`, `ExpiryPass` and the loop `RunLifecycle` |
| `teardown.go` | the workspace and fork collaborators |
| `cleanup.go` | `CleanupRecords`, the maintenance task `sandbox_record_cleanup` |
| `reconcile.go` | `Reconcile`, `ReconcileAtStart` and the loop `RunReconcile` |

The five routes are declared in `routes.go` through `WorkspaceRoute`, kind
write; a runtime failure is answered with `RuntimeError`, scrubbed of the
sandbox's values first, as `answerUse` does.

**Checkpoint.** It runs git in the sandbox through the runtime client's
`Exec`: `rev-parse` to find the work tree and its git directory, the local
branches and their heads, `status --porcelain`; the heads the workspace's
repository lacks are found with `gitrepo.Repo.Missing` on the repository
`Deps.Repos` opens (a fork's shares its parent's objects). Uncommitted
changes become one commit built in a temporary index copied from the real
one (`git add --all`, which honours `.gitignore`, `write-tree`,
`commit-tree` with `gitrepo.HubSignature` and no hooks or signing), so the
sandbox's index, branches and working tree are not touched; untracked
files over `CheckpointFileLimit` (100 MiB), which one `find` in the
sandbox names, are left out with an `:(exclude,literal)` pathspec. One `git bundle` of the kept heads, without
what the clone's `origin` already holds, is read with the runtime's
`Download` into a temporary file on the hub, at most
`Deps.MaxCheckpointSize` bytes (`[git].max_push_size`, which the hub's
composition reads with `gitserver.MaxPushSizeFrom`; 0 for no limit): at
one byte more the download stops and the attempt fails, as a pack over
the limit fails a push, so that a bundle never fills the hub's disk. It is fetched with
`gitrepo.Repo.FetchBundle`, which sets the attempt's refs under
`refs/parley/sandboxes/<id>/` in one ref transaction; earlier refs of
other branches stay, but for one whose name clashes with a ref of the
attempt as a directory and a file, which `FetchBundle` deletes in a
transaction before (git refuses both in one) and puts back when the
attempt's refs cannot be set. The attempts of one sandbox take turns from the fetch
to the record (`Service.checkpointing`, a lock set apart from
`Service.lifecycle`, which a stop or a deletion holds while it
checkpoints), so the refs and the stored checkpoint are one attempt's. The files an attempt
makes in the sandbox are named relative to the working directory, under its
git directory, and removed afterwards. The attempt is bounded by
`CheckpointBound` (5 minutes) on the hub's clock; the stored
`CheckpointState` and the event are written in one transaction, a failed
attempt keeping the refs of the last successful one; an attempt whose
caller's context ends is abandoned unrecorded. The refs never go through
the push path (`pushrule`), so no change, version or push event results.

**Start.** `StartSandbox` judges a start in one transaction, in the order
of a create's refusals (`workspace.RequireReady`, `fork.GuardUpdate`, the
resolution of the record's snapshot, `CheckQuota`), mints the replacement
hub credential (`MintHubCredential`, its value kept in memory before the
commit as a create's is), revokes the old one, records the hub's clock as
the last activity, so that the next idle pass leaves it, moves the record to
`provisioning` with the `start` marker and `parley.sandbox.start`, and wakes
the provisioner. A start's provisioning (`startSteps` in `provision.go`)
clears nothing at the runtime first, registers the providers again with
the values resolved now, calls `StartSandbox` with the composed policy and
the environment instead of `CreateSandbox`, waits until the sandbox runs,
rebuilds the working directory when `GetSandbox` reports it gone (the
checkout again, then `Restore` from the sandbox's own checkpoint), and
starts the main process again. Its failure keeps the runtime's sandbox
and its working directory: a best-effort `StopSandbox` releases the
compute, and the record ends `failed` with the step's reason.

**Restore.** `Restore` resolves each checkpoint ref in the workspace's
repository (a missing one fails, naming it), makes one bundle of them on
the hub (`gitrepo.Repo.CreateBundle`), uploads it into the clone's git
directory, fetches its objects with git in the sandbox, sets each branch
(a dirty one to the commit under the extra one), resets the checked-out
one, and for a dirty ref on the checked-out branch puts the extra commit's
tree into the working tree only (`git restore --worktree`), so the changes
come back uncommitted.

**Delete.** A deletion checkpoints a `ready` sandbox when asked, deletes
the runtime's sandbox and providers (`clearRuntime`: those labelled
`parley.sandbox=<id>` or named after it), logs and ignores a runtime
failure, and records in one transaction, which goes on after the caller
leaves, the move to `deleted`, the revocation of the hub credential and the
delete hooks (`RunDeleteHooks`). The purge hooks (`RunPurgeHooks`) run in
the transaction that removes a record: `CleanupRecords`, one transaction
per record deleted more than 24 hours before, and the remover of owned
data.

**Background work.** `composeSandbox` registers `RunLifecycle` as
`sandbox_lifecycle` and `RunReconcile` as `sandbox_reconcile`, each with a
grace of 15 seconds, and `ReconcileAtStart` as the start hook
`sandbox_reconcile`, after `sandbox_runtime_check` and
`sandbox_provisioner`; with sandboxes off they return at once, since no
runtime client is built. `RunLifecycle` runs `IdlePass` and `ExpiryPass`
every minute on the hub's clock, and `ExpiryPass` when a timer set to the
earliest `expires_at` of a live sandbox fires; the timer is re-armed on
every applied transition (the service's change notifier) and after a
change of `ttl`. `RunReconcile` runs a pass every
`[sandbox].reconcile_interval`: it lists the runtime's sandboxes and
providers, records exits, follows the runtime's restart of a sandbox with
the mark `runtime_provisioning` and no event, fails the `ready` records
(and the `provisioning` ones marked created) the runtime does not hold,
deletes the labelled objects of no live record, and ends with the idle and
expiry passes; while the listing fails it changes nothing but by expiry.
When their context ends, a pass stops before its next runtime call and a
checkpoint in progress is abandoned unrecorded; nothing at the hub's stop
calls the runtime.

**Collaborators.** `composeSandbox` registers the service on the workspace
service's `Teardown` set ([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md))
while it composes, whether sandboxes are on or not: as the
`SandboxRemover` (`RemoveSandboxes`: delete every sandbox of the slugs with
`workspace_deleted` and no checkpoint), an `ArchiveObserver`
(`WorkspaceArchived`: stop the `ready` sandboxes of the workspace and its
forks with `archived`) and a `Remover` of owned data
(`RemoveWorkspaceRecords`: in the removal's transaction, the purge hooks and
the removal of each record of the slug, and for a fork its refs under
`refs/parley/sandboxes/`). In the start hook `sandbox_records`, with
sandboxes on, it registers the service as the fork component's
`SandboxLifecycle` (`DeleteSandboxes`, `HasLiveSandbox`). The service is
its own `Restorer`, registered while the hub composes.
`Deps.SetTokenExpiry` is bound to
`identity.SetTokenExpiryByID`, through which a change of `ttl` moves the hub
credential's expiry.

## The network policy of sandboxes

The network policy of a sandbox is in `internal/sandbox` too, beside the
records it extends, and plugs into the provisioning's rule provider,
policy composer, delete hooks and purge hooks on the same
`*sandbox.Service` from `registerSandboxCollaborators`
(`internal/hub/sandbox.go`), whether sandboxes are on or off;
`composeNetworkPolicy` adds its tables and routes.

**Stores.** Five tables, each its own schema step: `sandbox_rules` (`RuleSchemaStep`: a workspace sandbox rule by id,
its workspace, the rule as given and its canonical form, summary,
findings, origin, creator and time; unique per workspace and canonical
form), `sandbox_approved_rules` (`ApprovedRuleSchemaStep`: the rule each
approval adds to its sandbox, ordered by decision time and an
autoincrement column), `sandbox_denials` (`DenialSchemaStep`: the
(program, host, port) triples reported per sandbox),
`sandbox_proposals` (`ProposalSchemaStep`: unique per sandbox and the
runtime's proposal id) and `sandbox_policy_log` (`PolicyLogSchemaStep`:
per sandbox, the time of the last policy log entry taken, in Unix
nanoseconds, and how many entries at that time were taken). The canonical form of a rule (`CanonicalRule`) is
its JSON re-encoded with sorted keys and no white space; two rules are
identical when their forms are equal. Rules of a workspace are never
read by the hub beyond that.

**Composition.** `RegisterNetworkPolicy` registers the rule provider (the
rules of the family root, read in the creating transaction, recorded with
the sandbox as `WorkspaceRules`) and the policy composer. Its `Compose`
(`ComposePolicy`) gives the snapshot's base policy, `ProtectedRules`
unchanged, and as added rules the recorded copies (named by rule id) then
the sandbox's approved rules (named by proposal id), leaving out an
approval identical to a copy. With a policy file it first asks the
runtime's `ValidatePolicy` which providers the base document refers to,
and refuses one that names the sandbox's hub provider. Its `Explain`, after
the runtime rejected the composed policy, validates the base policy with
the protected rules alone, then with the copies added one at a time, then
with the approved rules added one at a time after them, and names the
profile, the first copy's rule id or the first approved rule's proposal
id. Provisioning calls both for a create and for a start, so a stopped
sandbox's start gets the same policy without depending on this code. It
composes and creates or starts the runtime's sandbox in the sandbox's
decision turn (`Service.deciding`, see Decisions), so an approval never
falls between the composition and the runtime's call.

**Collector.** One background work, `sandbox_policy_collector`, runs
`CollectPolicy`: a pass right away and then every interval (5 s) on the
hub's clock. Each pass follows the policy log (`Logs`, source policy,
follow) of every `ready` sandbox on a follower goroutine and stops the
followers of the others; a follower counts each deny entry on
`DenialsMetric` and, in one transaction, inserts its triple, writes
`parley.sandbox.deny` only when the insert was new, and stores how far
the log was taken in `sandbox_policy_log`. A deny entry whose sandbox's
values cannot be known (`ValuesOf` fails) is not taken: the follower ends
and the entry is taken again after the retry delay, so no triple is
stored with its texts withheld. A broken stream is reopened after the
retry delay, from the time of the last entry taken (`Since`), skipping
entries already taken; the follower of a sandbox that is ready again, or
of a process after a restart, starts from the stored position, so an
entry is counted once, but for one taken again after its transaction
failed. Every pass but the first lists the runtime's proposals
(`ListProposals`), so a hub's start makes no runtime call beside its
check: a new one of a sandbox whose snapshot allows proposals is stored
`open` with `parley.sandbox.proposal`, its summary scrubbed of the
sandbox's values (`ScrubSandbox`) and its rule as the runtime gave it,
one whose snapshot does not is rejected at the runtime. The collector never returns an error and ends
with its context.

**Decisions.** Approve and reject run one function, `decide`. The checks
of a promotion that need only the caller and the route's workspace (the
switch, write access to the holding workspace through `Deps.Readable`, an
archived parent) come first; then a per-sandbox lock (`Service.deciding`)
is held from the sandbox read to the commit, and a promotion also takes a
per-holding-workspace lock (`Service.promoting`), always after the
sandbox's, before its identical-rule lookup and the limit of 50. A
provisioning takes the sandbox's lock alone, from before it composes the
policy until the runtime's create or start returned, so lock order is
always decision, then promotion. The runtime's `AddPolicyRule` is called
before the commit and only for a `provisioning` or `ready` sandbox, so a
decision refused by a check or by the runtime has changed nothing; the
commit is conditional on the proposal still being `open`. Once the
runtime has the rule, the commit runs on a context the caller's departure
does not end, with its own deadline; since the runtime has no operation
that removes a rule, a commit that fails all the same is logged with the
sandbox's and the proposal's ids. A rejection tells the runtime after its
commit, on a context the caller's departure does not end, and only logs a
failure.

**Clean-up.** The delete hook `WithdrawProposals` marks a deleted
sandbox's open proposals `withdrawn`; the purge hook `PurgeNetworkPolicy`
deletes its proposals, triples, log position and approved rules with its
record; and
`RemoveWorkspaceRules` is a `Remover` of owned data on the workspace
service's `Teardown` set ([ADR 07](adr/07-register-the-teardown-collaborators-on-the-workspace-service.md)),
which deletes a removed workspace's rules in the removal's transaction.

## parley

`cmd/parley` calls `cli.Run(ctx, os.Args[1:], os.Stdout, os.Stderr)` and passes
its result to `os.Exit`. `Run` is `cli.RunWith` with zero `cli.Hooks`;
`RunWith` lets a caller that runs the client in-process, such as an
end-to-end test, replace what it takes from the process (`Hooks.Stdin`, where
`parley login --api-key -` reads the credential; `Hooks.OpenBrowser`, which
opens the sign-in page, by default the system's opener in
`internal/cli/browser.go`; and `Hooks.LoginWait`, how long the browser login
waits for the redirect, by default 120 s). It builds a fresh cobra
command tree on each call: the root command `parley` with the persistent
global flags `--endpoint-url`, `--api-key` and `--json`, the `help`,
`version` and `login` subcommands and the groups `user`, `orgs` and `admin`,
plus the commands the unexported `options.extra`
builds, which tests use to add probe commands. It executes the tree with the
arguments; when a command returns an error, `run` writes it to stderr with
`printError` and returns `exitCode(err)` (both in `internal/cli/errors.go`).
Cobra's own error and usage output is silenced, so stderr holds the one
envelope.

A command is declared through one helper, `declare` in
`internal/cli/command.go` (see
[ADR 02](adr/02-declare-every-cli-command-through-one-helper.md)): a
`command` value with its last word, its description, the route it calls
(`Method` and `Path` relative to `/api/v1`, empty when it calls no single
route), its named arguments (`argSpec`), its `auth` level (`none`, `user`,
`admin`), its own flags and its run step. `declare` keeps the declaration as
JSON in the cobra command's `Annotations` (`parley.declaration`), refuses a
wrong number of arguments, and a flag marked required with
`cobra.MarkFlagRequired` that is not given, as a `usage_error`
(`checkCommandLine`, which runs before cobra's own check of required flags,
whose error has no kind), and runs the run step with a
`call`: the context, the arguments by name, stdout, stderr and the run's
options, which `run` passes down in the context. `group` builds a command
group, which prints its help without a word and refuses a word that is none
of its children as a `usage_error` (cobra alone would print the help); the
root is a group too, and its flag error handler turns every flag that is
unknown or does not parse, on any command, into a `usage_error`.
`parley version` and `parley login` are declared through `declare`.

The commands of the user and organization routes are declarations without
client code of their own: `parley user` (`internal/cli/user.go`), `parley
orgs` (`orgs.go`), `parley admin users` (`admin_users.go`) and `parley
admin orgs` (`admin_orgs.go`), the last two in the group `parley admin`
that `newAdminCommand` in `root.go` builds. Those that only call their route
use the default run step; a command whose request carries a query or a body
(`--include-blocked`, `--full-name`, the flags of `parley admin users
create`, and those of `parley admin orgs create` and `update`) has a run
step that builds them from its flags and sends them with `sendWith`
(`user.go`), the default run step with a query and a body. The two `list`
commands with `--include-blocked` share the run step `sendIncludeBlocked`
(`admin_users.go`), and `parley admin orgs create` and `update` send a
member only when its flag is given (`addChanged` in `admin_orgs.go`).

`parley admin keys` (`admin_keys.go`, also in `parley admin`) is declarations
alone too. `parley keys` (`keys.go`) is `list`, a declaration, and `refresh`
and `revoke`, whose run steps take the route's `:key_id` from the command's
own credential (`apiKeyID`, which repeats the hub's API key format, since
the client does not import the hub's code) and refuse any other credential
before sending. After the hub's answer is printed, `refresh` saves the new
key and `revoke` clears `api_key` and `user_id` through `saveConfig`, both
only in a config file that already exists, so neither creates one.

`parley admin tokens` (`admin_tokens.go`, also in `parley admin`) is
declarations alone. `parley tokens` (`tokens.go`) is `list`, `show` and
`revoke`, declarations, and `create`, `replace`, `add` and `remove`, whose
run steps build the body from the flags: `splitList` splits `--permissions`
and each `--paths` value at every comma, without the quoting of pflag's
comma-separated flags, and `create` sends `paths` only when `--paths` is
given and declares `ShowsOnce`. `changePermissions` builds the three
permission commands; after the answer is printed, `replace` and `remove`
note on stderr a Token whose `revoked_at` is not null.

`parley secrets` (`secrets.go`) is `create`, `list`, `update` and `delete`,
and `parley vars` (`vars.go`) the same four and `resolve`; both collections
share the run steps of `secrets.go`, each given its collection (`secrets`
or `vars`): `createEntries`, `listEntries`, `updateEntry` and
`deleteEntry`. `withScopeArgs` wraps a command's argument
check, through `withArgsCheck`, so that the scope flags and the entries are refused as usage errors
before any setting is resolved: an empty `--org` or `--workspace`, more
than one scope flag on all but `create`, and arguments that `parseEntries`
or the `--from-stdin` check refuses. `scopesOf` turns the flags into the
scopes in the order user, org, workspace, the user's alone without
a flag, each giving its route for a collection. `parley vars resolve` takes
one workspace slug and no scope flag, and reads the resolved list with
`printWholeList`. `create` sends one request per scope, keeps going after a failure,
prints the items of the scopes that succeeded, writes every failure but the
last with `printError` and returns the last, which `run` prints, so each is
written once and the exit status is the last failure's. `list` reads the
whole list with `printWholeList`, which asks for pages of 200, follows
`next_cursor` and prints `{"items":[…]}`; `delete` prints its `Confirm`
document `{"deleted":"<KEY>"}` (`deletedKey`).

`parley workspace` (`workspace.go`) is `create`, `list`, `get` and
`update`. `get` is a declaration alone. `list` sends a query parameter for
each flag given (`listWorkspaces`) and prints one page. `create` and
`update` build their bodies from their flags (`workspaceCreateBody`,
`workspaceUpdateBody`) and wrap their argument check so that the body is
built there too: a command line it refuses (an update with nothing to
change, a `--setting` without `=`) is a usage error before any setting is
resolved. Both read `--setting`, registered by `addSettingFlag` as a string
array, through `addSettings`, which parses the values with
`ParseSettingFlags`, the one parser of `--setting`. `create --wait` polls
`GET /workspaces/<slug>` through the shared wait.

`parley fork` (`fork.go`) is `create` and `list`. `create` builds its body
from its flags with `forkCreateBody`, which its wrapped argument check calls
too, so that `--token-paths` without `--token-permissions` and words after
`--` without `--sandbox-profile` are usage errors before any setting is
resolved; the check counts only the arguments before `--` (cobra's
`ArgsLenAtDash`), and the words after it become `sandbox.command`. It does
not declare `ShowsOnce`, whose warning is unconditional: after printing the
answer it warns on stderr only when the answer holds `token.token`. `list`
sends a query parameter for each flag given (`listForks`), `--mine` as
`owner=me`, and prints one page.

`parley change` (`change.go`) is `open`, `list`, `show`, `find`, `versions`,
`diff`, `edit`, `ready`, `draft`, `abandon`, `restore` and `wait`. The five reads are built like the reads of `parley scm`
(`scm.go`), as an `scmRead` each, whose query flags become query
parameters; `diff` shares with `parley scm diff` the check `onePatchForm`
and the run step `diffRevisions`, which prints the patch text of `--patch`
as received. `open` builds its body in `openChange`, a member only for a
flag given, and reads `--intent @<file>` or `-` through `intentText`
before sending, a file that cannot be read being a `client_error`.
`withNumbers` wraps the argument check of `open`, `list`, `show`,
`versions` and `diff`, so that a `<number>`, `--depends-on`, `--stack-of`,
`--version` (or `latest`) or `--against` that is not a positive integer is
a usage error before any setting is resolved; it wraps the five edits'
`<number>` too. `edit` builds its `PATCH` body in `editChange`, a member
only for a flag given and the intent through `intentText`; a label flag
makes it read the change first and send the list `editLabels` computes
from the change's `labels`. `ready` and `draft` (`newChangeStateCommand`)
send only a `state`, `abandon` only the `reason` and `dependants` given,
and `restore` no body. `checkFlags` wraps the argument check of `edit` and
`abandon` with their flag rules (an edit flag needed, `--auto-land` with
`--no-auto-land`, a `--dependants` value), refused as usage errors before
any setting is resolved; it wraps `wait`'s for its `--for` value.
`wait` (`waitForChange`) does not use the shared wait of `wait.go`, which
starts an operation and reads one state member: it polls the change with
`GET` at once and then one poll interval after each answer, under one
deadline counted from its start, and decides each answer with
`changeWaitOutcome` from `--for`, `state`, `landing.status` and
`mergeable`. It takes from `wait.go` the duration flags
(`newDurationValue`), the tests' `pollInterval` and `waitEnded`, which
turns a passed deadline into `wait_timeout`.

`parley change land` and `parley change lands`, and the group `parley
land` (`list`, `show`) that `newLandCommand` builds and `root.go` adds to
the root, live in `land.go`. `change land` declares `Wait`, so the shared
wait of `wait.go` runs it: its `Start` sends the body `landBody` builds
from the flags given, its `Poll` reads the attempt's `id` and asks for
`GET /workspaces/<slug>/lands/<id>`, and `byState` classifies `status`
(`landed` a success; `conflict`, `blocked` and `failed` failures). It
declares `Timeout` 70 seconds (`landRequestTimeout`), above the 60 seconds
the hub may hold a land, and `withNumbers` and `checkFlags` refuse a
`--expected-version` that is not a positive integer and a blank
`--override`. The reads are `scmRead`s.

The group `parley policy` (`get`, `set`, `evaluate`), which
`newPolicyCommand` builds and `root.go` adds to the root, lives in
`policy.go`. `checkFlags` wraps each command's argument check:
`nonNegativeFlags` refuses a `--revision`, `--if-revision` or `--change`
that is not a non-negative integer (`withNumbers` takes positive ones
only, and a revision or a change number may be 0), `evaluate` takes
exactly one of `--paths` and `--change`, and `readPolicy` reads the file
of `set --file` and `evaluate --policy-file` there too, so that a file
that cannot be read or is neither JSON nor TOML is a usage error before
any setting is resolved. `readPolicy` reads `-` from standard input, a
name ending in `.toml` as TOML, anything else as JSON and, when it is not
valid JSON, as TOML, whose decoded document is sent as JSON (integers stay
numbers, booleans booleans); a JSON document is sent as it is, for the hub
to judge. `get` sends `--revision` as the query `revision`; `set` sends
the document with `PUT` and `--if-revision <n>` as `If-Match: "<n>"`
through the request's own `header`; `evaluate` sends a member only for a
flag given, the `--paths` values split at commas in command-line order.

Help lives in `internal/cli/help.go`. `installHelp` keeps cobra's own text
help, sets the root's help function, which `--help` on any command and a
group run without a word reach, and replaces cobra's help command with
`help [command]...`, declared through `declare` (auth `none`, no route; its
argument `Repeats`, taking every remaining word). The help command finds its
words with the root's `Find`; words left over are a `usage_error` naming
the first, and otherwise it prints what `--help` on that command prints,
after adding cobra's `--help` flag to it as cobra does when it runs it. With
the global `--json` the help is one JSON document instead: `describe`
builds a command's object from its declaration (`declarationOf`, which
decodes the `parley.declaration` annotation; an undeclared command has an
empty description and auth), with `method` and `path` `null` when empty and
`flagsOf` listing its local and inherited flags sorted by name, without the
flags cobra adds itself; for a group (`isGroup`, the `parley.group`
annotation), the root included, `describeAll` lists every command under it
that is not a group, at any depth, sorted by name. Help resolves no setting,
reads no file and sends no request.

Before the run step of a command whose `auth` is `user` or `admin`,
`declare` resolves its settings with `resolveSettings`
(`internal/cli/config.go`): the hub URL from the first non-empty of the
global flag `--endpoint-url`, `PARLEY_ENDPOINT_URL` (these two through
`givenHubURL`, which `parley login` and `parley version` share) and the
file's `endpoint_url`, the credential from `--api-key`, `PARLEY_API_KEY` and
`api_key`. It calls `readConfig` only when the flags and the environment
leave one empty. A missing setting (a missing hub URL with the message of
`noHubURL`, which `parley login` shares), a file that cannot be parsed and
a hub URL that is not an absolute `http` or `https` URL with a host and
without a query or fragment (`checkHubURL`) are `client_error`s, returned before the run step starts,
so no request is sent. The resolved settings build the call's `client`
(`internal/cli/client.go`) with `call.newClient`, which gives it the
declaration's `Timeout` or else `options.requestTimeout`
(`defaultRequestTimeout`, 30 s; tests shorten it). A command whose `auth` is
`none` gets no client: it needs no credential and builds one itself for the
hub URL it resolves.

`client` is the one HTTP client of the commands, `parley login` and
`parley version` included. `newClient(hubURL, credential, timeout)` drops
the URL's trailing slashes. An `apiRequest` holds the method, the path
(relative to `/api/v1`, or with `outsideAPI` to the hub URL itself, as
`GET /version` is), the query, the body, an optional timeout of its own and
optional headers of its own (`header`, such as the `If-Match` of `parley
policy set`). `raw(ctx, r)` sends it to `<hub URL>/api/v1<path>` with
those headers, then
`Authorization: Bearer <credential>` (none when the credential is empty),
`Accept: application/json`, and the body encoded as JSON with
`Content-Type: application/json` only when there is one, which take
precedence over a request header of the same name, and returns the
status and the whole body whatever they hold, for a command whose output is
not one JSON document. The timeout is a context around connecting, sending
and reading the whole body; when it runs out the answer is a `client_error`
(`the hub at <hub URL> gave no complete answer within <timeout>`), and any
other failure is a `client_error` naming the hub URL: to connect or send,
`cannot reach the hub at <hub URL>: <reason>`; to read the answer,
`cannot read the answer of the hub at <hub URL>: <reason>`. `send(ctx, r)` is `raw`
that accepts only an answer of the REST API: a status of `400` or above
whose body is an object with an `error` member holding `error_type`
(`isHubEnvelope`, in `errors.go`) becomes a `hubError`; any other status
outside `2xx`, and a `2xx` body that is neither empty nor JSON, a
`client_error` naming the status. `request(ctx, method, path, query, body)`
is `send` for the common case. Each client has its own transport, a clone
of `http.DefaultTransport` with keep-alives disabled: Go's transport can
send a request again on a new connection when a reused one breaks, and the
hub may have received it the first time, so every connection serves one
request and nothing in the client retries one. Its `CheckRedirect`
(`keepRedirect`) returns a redirect as the answer instead of following it,
which would send the request again (a `POST` with its body after a `307`
or `308`); `send` then refuses it as a status outside `2xx`.

`internal/cli/errors.go` holds the two kinds of error a command ends with.
`hubError` is an error envelope the hub answered, with the HTTP status and
the body's bytes as received, which `printError` writes indented again by
two spaces with `json.Indent`, so its member order and extra members stay
as the hub sent them. `cliError`
is an error the CLI raises itself, built by `usageError`, `clientError`,
`waitFailed`, `waitTimeout` and `conditionNotMet`; `printError` writes it as
`{"error":{"code":0,"message":…,"error_type":…}}` indented by two spaces,
and any other error as a `client_error` with its text as the message.
`exitCode` maps no error to `0`, a `hubError`, `wait_failed` and
`wait_timeout` to `1`, `condition_not_met` to `3` and everything else to
`2`. `parley login` returns these errors too.

`internal/cli/output.go` prints what a command answers. A command declared
without a run step gets the default one, `sendAndPrint`: it fills each
`:param` segment of `Path` with the argument of that name, percent-encoded
as one segment (`routePath`, `pathSegment`: `url.PathEscape`, and `.` and
`..` as `%2E` and `%2E%2E`), sends `Method` to it with the call's client's
`send` and passes the success to `printAnswer`. A `2xx` answer without a
body is replaced by `{}`, or by the document the declaration's `Confirm`
builds. `printResult` then writes the result with `writeResult`, which
compacts the body and indents it by two spaces with `json.Indent`, never
decoding it, and adds one newline; writes the line `warning: this value is
shown once and will not be shown again` to stderr for a declaration marked
`ShowsOnce`; and, for each of the declaration's `FailOn` conditions whose
flag `--fail-on-<Name>` (registered by `declare`) is given, returns
`condition_not_met` with the condition's message when `Check` says it holds
in the result, so the result is on stdout before `run` prints the error.
`warn` and `progress` write a plain line to stderr, a warning prefixed with
`warning: `; `parley version` writes its warnings with `warn`. `parley
login` prints the user with `printResult`.

`internal/cli/wait.go` holds the shared wait for a long operation. A
command declares `Wait`, a `waitFor`: `Start` (optional; the request that
starts the operation, by default `Method` to `Path`), `Poll` (the request
that asks for the operation's state, built from the record last received),
`Classify` (a record's state and whether it is not terminal, `successful`
or `unsuccessful`; `byState` builds one from a string member and the lists
of successful and unsuccessful states) and `Document` (optional; the
document printed in place of the final record, from the start's answer and
the final record). `declare` registers `--wait` (bool), `--timeout` and
`--poll-interval` (`durationValue`, a `pflag.Value` whose `Type` is
`duration`, defaults `5m` and `5s`) and makes `call.wait` the run step of a
command that declares no `Run`. `durationValue.Set` parses with
`parseDuration` (a whole number above 0 and one of `s`, `m`, `h`, `d`), so
a value it refuses is a flag that does not parse, a `usage_error` from the
root's flag error handler before any request, with or without `--wait`.
The client does not import the hub's `internal/server/timefmt`; the parser
is its own. `call.wait` sends the start request through the call's client;
without `--wait` it prints the answer with `printAnswer`. With `--wait` it
classifies the start's record, writes `waiting: <state>` with `progress`,
and until a record is terminal waits one poll interval (or the
`options.pollInterval` tests set below one second), sends the poll under a
context whose deadline is `--timeout` after the start's answer came, and
writes `waiting: <state>` when the state changes. The final record, or
`Document`'s document, is printed with `printResult`; an unsuccessful end
then returns `wait_failed` naming the state. When the deadline passes,
while sleeping or with a poll in flight (the client's error then names the
abandoned request, not the reason), `waitEnded` returns `wait_timeout`;
any other error of a poll (the hub's envelope, a `client_error`, a poll's
own request timeout before the deadline) ends the wait as it is. Nothing
is printed on stdout then, and no request cancels the operation.

`internal/cli/config.go` is the one implementation of the client
configuration file. `configPath` gives `$PARLEY_CONFIG_DIR/config.toml`, or
`~/.parley/config.toml`, and an error when it needs the home directory and
`os.UserHomeDir` has none. `readConfig` returns the file's `endpoint_url`,
`api_key` and `user_id` as a `configSettings`; a missing file or no path
gives empty settings, and a file that cannot be read or parsed is a
`client_error` naming the path. `saveConfig` takes a `configSettings`,
decodes the existing file with `github.com/BurntSushi/toml` into a map, so
that keys it does not know survive, sets the three keys and encodes the map;
it writes a temporary file in the same directory (created with mode `0700`
when missing), syncs it, sets its mode to `0600` and renames it over
`config.toml`. Every failure, including a file it cannot parse, is a
`client_error` naming the path, with the temporary file and the directories
the save created removed, and the old file left as it was; without a path, because there is no home directory,
it is `cannot save the config file: <configPath's error>`.

`parley login --api-key`, with the global flag (`internal/cli/login.go`),
takes the credential with `loginCredential` (an empty one is a
`usage_error` before anything is read or sent), reads the client
configuration file with `readConfig`, resolves the hub URL
from the flag, `PARLEY_ENDPOINT_URL` and the file, and asks the hub who the
credential belongs to with `GET /api/v1/user` through a `client` that
presents it (30 s timeout). On a `2xx` answer it saves `endpoint_url`,
`api_key` and `user_id` with `saveConfig`. A hub URL that is not an absolute
`http` or `https` URL with a host and without a query or fragment is
refused with `checkHubURL`, as for every command.

Without `--api-key`, `parley login` signs in through the browser
(`browserLogin` in `internal/cli/login.go`): it validates `--expires`,
reads the config file and resolves the hub URL as above, takes the
provider's `authorize_url` from `GET /api/v1/auth/providers`, listens on
`127.0.0.1:0`, and opens the `authorize_url` with `redirect_uri` (that
listener's `/callback`) and a 32-hex-character `state` from `crypto/rand`
added. A `net/http` server on the listener serves `GET /callback` once: it
checks the state in constant time, answers the browser with an HTML page,
and hands the code back; the server is shut down and the listener closed on
every way out, including the wait's expiry. The code goes to
`POST /api/v1/auth/callback` without a credential, and the answer's key and
user are saved as in the key flow. Both flows send their requests through
the shared `client` with its `request` and `send`; the requests before the
credential is known carry no `Authorization` header, and the callback states
its own timeout of 60 s. A hub's error envelope comes back as a `hubError`,
as it was received.

`parley version` reads its build from `internal/buildinfo`, takes the hub URL
from `--endpoint-url`, `PARLEY_ENDPOINT_URL` and then `readConfig` (a file it
cannot parse is a warning, not a failure), and asks the hub for its build
through the shared `client` (`GET /version` with `outsideAPI`, no
credential, its own 5 s timeout from `options.versionTimeout`); every
failure of that request, and an answer other than `200` with a JSON object,
is a warning. `internal/cli` imports `internal/buildinfo` and nothing from
`internal/server`, so the client carries no server code. The commands, output
and exit statuses are described in [cli.md](cli.md).

## Build information

`internal/buildinfo` has three package-level string variables, `Version`,
`Commit` and `BuildTime`, that are set when the binary is linked, with one `-X` linker flag each:

```sh
go build -ldflags "\
  -X github.com/agent-fox-dev/parley/internal/buildinfo.Version=1.4.0 \
  -X github.com/agent-fox-dev/parley/internal/buildinfo.Commit=3f2a1bc \
  -X github.com/agent-fox-dev/parley/internal/buildinfo.BuildTime=2026-01-02T03:04:05Z" \
  ./cmd/parleyd
```

The same flags build `./cmd/parley`. `test/e2e` builds both programs this way
and checks that `parley version` reports the values.

`buildinfo.Get()` returns them as a `buildinfo.Info`, with the Go toolchain's
version (`runtime.Version()`) as `GoVersion`. A value that was not set is
reported as `dev` (version) or `unknown` (commit and build time); a value that
was set is reported unchanged. `GET /version` calls `Get()` on every request.

The package lives under `internal/` and outside `internal/server` so that the
client can report its own build without importing server code.
