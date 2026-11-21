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
- `internal/scm` exports three small helpers for the reads this package
  serves: `CheckPath`, `Revalidated` and `Uncached`.
