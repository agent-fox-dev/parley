# Errata: 20_scm_read_api

Where the implementation of package 20 (`.specs/20_scm_read_api`) differs
from what the package says, or decides what it leaves open.

## The compare route needs git 2.40

The package names git 2.38 as the floor and says that its `merge-tree
--write-tree` with an explicit merge base does one step of the trial rebase.
`merge-tree` takes `--merge-base` only from git 2.40, so the compare route
(`gitrepo.(*Repo).TrialRebase`) needs git 2.40 or later; every other read
keeps the 2.38 floor. `merge-tree` takes trees in place of commits only
from git 2.45, so the trial writes a parentless commit of each merged tree
(`git hash-object -t commit -w`) to replay the next commit onto, and a
commit of the empty tree as the merge base of a root commit. These commits
are objects no ref reaches, which the package allows.

## The delete/modify conflict is checked against another target

The check of the conflict report compares four heads with `main`, each
conflicting on `f.txt` in one of four ways. With one `main` that cannot
hold: `content`, `add_add` and `modify_delete` need `f.txt` on `main`, and
`delete_modify` (the target deleted a file the head changed) needs it gone
from the target, `ours` being `null`. The tests
(`TestCompare_ReportsTheConflictOfTheFirstCommitThatDoesNotApply` and
`TestTrialRebase_ReportsTheFirstCommitThatDoesNotApply`) compare the head
`base-deletes` with `trimmed`, which is `main` and a commit deleting `f.txt`;
the other three heads are compared with `main`, and every assertion of the
check is made on all four.

## A search query holding a NUL character is refused

The package accepts any `q` of 1 to 500 characters. git takes the pattern
as a process argument, which cannot hold a NUL byte, so a `q` holding the
character U+0000 is `400 invalid_request` (`q must not contain a NUL
character`) before the workspace's state is checked and before any git
runs (`internal/scm/search.go`, `gitrepo.ErrInvalidPattern` in
`internal/gitrepo/grep.go`). A NUL could only match a line of a file whose
first 8,000 bytes hold none, since git skips any other as binary.

## A search's regular expression is the same on every platform

The package says `regex=true` takes `q` as an extended regular expression,
and its check uses `\w`, which POSIX leaves undefined: git hands the
expression to the C library's `regcomp`, and macOS's does not know `\w`
where glibc's does. `gitrepo.(*Repo).Grep` writes `\w`, `\W`, `\s` and
`\S` outside bracket expressions as `[[:alnum:]_]`, `[^[:alnum:]_]`,
`[[:space:]]` and `[^[:space:]]` before git sees the expression, so they
mean what they mean with glibc on Linux and on macOS alike. Other GNU
escapes, such as `\b`, `\<` and `\>`, are left to the C library.
