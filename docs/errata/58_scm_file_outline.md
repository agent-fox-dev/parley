# Errata: 58_scm_file_outline

Where the implementation of package 58 (`.specs/58_scm_file_outline`)
differs from what the package says, or decides what it leaves open.

## The image test reads how the image's programs were built

The package's check of the hub image makes a git repository with `a.py`
inside the container, creates a workspace from it through the hub's API
with the admin token, and asks the hub in the container for the outline of
`a.py`. The hub refuses both halves of that: the admin token cannot create
a workspace (`403 forbidden`, `a workspace needs a user as owner; the admin
token cannot create one`), and a workspace's `git_url` has the form
`https://<host>/<path>` or `git@<host>:<path>`
(`workspace.ValidateGitURL`), so a repository on the container's own disk
is no origin. The hub also runs git without the system and global
configuration, so no `insteadOf` rule can point such a URL at a directory.

The image test (`TestHubImage_RunsTheHubOnOneVolume` in
`test/image/hub_test.go`) therefore copies `parleyd` and `parley` out of
the running hub container and reads their build information: `parleyd` is
built with `CGO_ENABLED=1` and links the tree-sitter grammar modules,
`parley` is built with `CGO_ENABLED=0` and links none. The same test's
other parts show that this `parleyd` starts and serves on the runtime
stage. That a cgo build outlines the fifteen languages is shown outside
the image, by `TestOutline_OutlinesEveryLanguageOfTheList` in
`internal/scm/outline_cgo_test.go`. No test asks a hub running in the image
for the outline of a file in a language other than Go.

## A NUL byte between byte 8,000 and byte 8,192

The route calls a file binary as blame does, by a NUL byte in its first
8,000 bytes, and answers `422 binary_file`. agentkit-go's `outline` looks
at the first 8,192 bytes and outlines no file with a NUL byte there, so a
file whose first NUL byte lies in the 192 bytes between the two is answered
`200` with `supported: false` and no declarations.

## A parse is left behind at the read timeout, and four run at once

The package ends a read that outlasts `[scm].read_timeout` and leaves the
end of the parse to the request's context. agentkit-go's `outline` looks
at the context only while tree-sitter parses: its Go backend and its walk
over the declarations it found do not, so a parse can outlast the timeout.
The route answers `503 scm_timeout` at the timeout all the same and leaves
the parse to end by itself; so that such parses cannot pile up, at most
four files are parsed at once (`scm.DefaultMaxOutlines`), a parse left
behind included, and a read that finds no place within its time is
answered `503 scm_timeout` too. Test:
`TestOutline_AnswersATimeoutWhenTheParseOutlastsTheReadTimeout`.

## An answer is cached as every read is, whatever the build

The answer's entity tag comes from the commit and the request, and a read
by a full commit id is marked immutable, as the package asks. `supported`
also depends on whether the hub was built with cgo, so an answer a client
kept from a hub built without it stays `supported: false` for that client
after the hub is replaced by one built with cgo. The hub image is built
with cgo, so this concerns only an operator who ran a build without it.

## Smaller choices

- A file that does not parse in its language is outlined as far as its
  syntax tree goes, as agentkit-go answers it; it is no error.
- The end-to-end test of the package's flow expects `supported: true` for
  `app.py` only when the test binary's outline package has a tree-sitter
  backend, so the suite also passes with `CGO_ENABLED=0`.
- `test/acceptance/routes_test.go`, the route table generated from the
  PRD's route index, does not list the outline route: the index does not.
