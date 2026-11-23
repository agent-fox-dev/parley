# Errata: 61_scm_declaration_operations

Where the implementation of package 61
(`.specs/61_scm_declaration_operations`) differs from what the package
says, or decides what it leaves open.

## The shared read rows read demo, which gained a Go file

The check of the rules every read shares adds the declaration route's row
to `scmRoutes` and puts a Go file in the history `addHistory` builds. The
rows of `scmRoutes` are read on the workspace `demo`, not `hist`, so the
Go file `g.go` (declaring `Hello`) was added to demo's second commit
(`demoStream` in `internal/scm/helpers_test.go`), and the row reads
`/declaration?path=g.go&name=Hello`.

## A parse that yields no syntax tree is the hub's failure

agentkit-go's outline answers a file whose backend returned no tree at
all, or whose grammar did not load, as `NotParsedFailed`. The package does
not name the case. The read and the declaration operations answer it as an
internal failure (`500`), since it is the hub's, not the caller's
(`outlineSource` in `internal/scm/declaration.go`).

## A test-first deviation for the consequences of a declaration commit

The checks of the criteria that a declaration commit keeps the
consequences of a commit (its events and answer, the empty-commit rule,
path-scoped tokens, the moved-head check, the change's version) pass
before the task that owns them changes any code: the task before turns a
declaration operation into a `put` ahead of the tree builder, and the
parent check of package 21 runs before it. They were recorded with
`no_red`.

## A missing path is `invalid_request`, as REQ-2.7 says

The PRD's text says a declaration operation without `path` is "the
existing `400 invalid_operation`"; the criterion that owns malformed
declaration operations, and the check of a `put` without a path, answer
`400 invalid_request`. The code follows the criterion
(`fileOperation.plan` in `internal/scm/declaration.go`).

## Smaller choices

- A declaration operation's content is inserted after a last line that
  has no line ending on a line of its own: the file's line ending is put
  between them, so that the two lines are not joined.
- A declaration operation reads its file as the file operations before it
  would leave it if they apply. When it is refused, those operations are
  applied first, and one that does not apply is the answer
  (`invalid_operation` with its index), so a request is refused for its
  first operation that fails.
- The put of a declaration operation replaces the put of an earlier one on
  the same file when nothing between them touches the file, so a request
  of many operations on one file holds one result of it.
- A refusal of a declaration operation is answered as it is even when the
  read timeout ends meanwhile; only an error the deadline caused is
  `503 scm_timeout`.
- The `parley scm commit` declaration flags take the local file after the
  first `=` that follows the last `#` of the value, as the package's task
  says; a local file whose name holds `#` cannot be named.
- The check of the commit timeout uses a read timeout of 300 ms and an
  answer within 3 s, not 100 ms and 1 s, so that the git reads before the
  parse fit on a loaded machine.
