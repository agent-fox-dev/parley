# Errata: 60_code_search_and_symbols

Where the implementation of package 60 (`.specs/60_code_search_and_symbols`)
differs from what the package says, or decides what it leaves open.

## Symbols are answered by path and line

The package answers symbols "in the index's order", cut to `limit`.
agentkit-go's `Symbols` collects its matches by walking a map of files, so
its order changes from one request to the next, and a `limit` would cut a
different set each time. The hub sorts the matches by path, then by first
line, then by name, and cuts that.

## A path that names a file

`Symbols` takes a directory and matches the files under it; a file's path
matches nothing. For a `path` that names a file of the indexed tree the hub
asks for the file's directory and keeps the declarations of that file. A
path that names nothing in the tree answers `symbols: []`.

## A build's directory stays while a query reads its index

The package holds the index open for the length of a query. That is not
enough: `codesearch` reads the tree its index was built from when it
answers, and with the tree removed it finds every file gone and answers no
file, with `200`. A query therefore pins the directory of the build it
reads, and the sweep that removes the builds a newer one replaced leaves a
pinned one until its last query has ended. The test of searches against
concurrent commits found this.

## A head the branch reached after arrival is told by its history

The package lets an index of "a head the default branch reached after" the
arrival head answer, and has the hub compare the record with the arrival
head and the branch's current head. Under commits that arrive faster than
builds end, an index is of a commit that is neither, and a request would
wait for a build that is always one behind. The hub also accepts a commit
that has the arrival head in its history (`git merge-base --is-ancestor`,
read in process). A head that was moved to a commit outside that history,
by a forced ref move, answers once it is the branch's current head.

## truncated is also true for an answer the index cut by size

`truncated` is "true exactly when more files matched". agentkit-go's
`code_search` also cuts an answer whose matching text is over 50 KB, drops
whole files for it and reports that as truncated; the hub has no other way
to ask. Such an answer holds fewer files than `limit` and says
`truncated: true`.

## A workspace without a head is 404 ref_not_found

The package answers such a workspace "as `GET …/scm/tree` without a ref
answers for it". That is `404` `ref_not_found`, with the message
`revision "" names no commit`.

## Smaller choices

- A file that matches by its name alone (`file:handler`, `lang:go`) has
  one match, line 1 with its path as `text`: it is what the index's tool
  answers for it, and it keeps `matches` from being empty.
- The table of queries the index cannot parse names `lang:`; zoekt parses
  it and matches nothing. The test uses `a)` in its place.
- `Retry-After` of a failed build is counted on the hub's clock from the
  failed job's next attempt.
- A `304` is answered after the index is loaded and before it is asked;
  loading is what a first query after a restart pays, tagged or not.
- A symbols request to a partial index is refused from the record, before
  the index is loaded.
- An index that goes away between the waiting rule and its use (its
  setting was turned off, its files could not be loaded) is answered
  `503 code_index_not_ready` with `Retry-After: 5`, not an internal error.
- A `path` that names a file is judged against the 1,000 declarations by
  its directory, since that is what the index is asked for.
- `internal/scm` exports three small helpers for the reads this package
  serves: `CheckPath`, `Revalidated` and `Uncached`.
