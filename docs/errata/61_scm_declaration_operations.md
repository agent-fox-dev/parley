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

## Smaller choices

- A declaration operation's content is inserted after a last line that
  has no line ending on a line of its own: the file's line ending is put
  between them, so that the two lines are not joined.
- A declaration operation's own index is checked before the
  operations before it are applied by the tree builder; a request whose
  earlier file operation does not apply and whose later declaration
  operation is refused answers the declaration operation's refusal.
