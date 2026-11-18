# Errata: 42_forge_writes

Where the implementation of package 42 (`.specs/42_forge_writes`) differs from
what the package says, or settles what it leaves open.

## The forge client sends a body

The package builds the writes on package 41's client, whose `forge.Request`
had no body. `forge.Request` gains `Body`, sent as JSON with
`Content-Type: application/json` (`internal/forge/client.go`).

## The write order uses the route helper's two hooks

The package names "the hook the route helper leaves for the archived and
credential checks". The helper has two: `Params`, before the forge is
resolved, and `Check`, after. A write route (`writeRoute` in
`internal/forge/write.go`) runs its path and body rules and then the
archived checks in `Params`, and the text limits, its forge-kind rules and
the credential check in `Check`, which gives the package's order with no
change to the helper's own steps.

## Handlers return their answer; the frame records, then writes

The event is recorded before the response is written, so a write route's
`serve` returns a `written` value (status, body and the event's details, also
next to an error) instead of writing to the response. `serveWrite` records
the event through `forge.Deps.Events` (an `EmitNow` emitter that
`internal/hub/forge.go` sets from the event component) and then answers. A
failed forge call is mapped there once with `MapError`. Whether a write
reached the forge is counted by `Call.sent` (`internal/forge/route.go`),
which `Call.send`, `Call.sendJSON` and `Call.Get` increment; a forge request
sent another way would record no event.

## The read mappings are shared, not copied

The package says a written comment has the URL its read gives. The comment
and pull request mappings of `github.go` and `gitlab.go` are methods
(`githubComment.comment()`, `gitlabNote.comment(page)`,
`githubPull.pull()`, `gitlabMergeRequest.pull()`) that the reads and the
writes both call. A GitLab note on a merge request gets its URL on the merge
request's page (`/-/merge_requests/<n>#note_<id>`), which the read mapping
did not build before because it read issue notes only.

## The write body limit is 16 MiB

The package asks that a text of exactly the forge's limit be accepted. The
platform's default body limit (1 MiB) is below a GitLab text of 1,000,000
characters, and a client that escapes characters beyond the Basic
Multilingual Plane as JSON surrogate pairs sends twelve bytes per character.
Every write route's `server.Route.MaxBodySize` is 16 MiB.

## A label name of dots is escaped

A label name is one escaped path segment of the forge request. `url.PathEscape`
leaves `.` and `..` as they are, which a forge or proxy that normalises paths
would read as the issue or the repository; such a name is sent as `%2E`
(`labelSegment` in `internal/forge/label_write.go`), as the CLI's
`pathSegment` does.

## What the package leaves open

- An ensure whose create is refused because the label exists looks the label
  up again; a second lookup that fails with anything but `404` is mapped and
  answered like any forge failure.
- A label event (`label.add`, `label.remove`) carries `number`, the issue's;
  only `label.create` has none, as the package's event rules say.
- `parley issue unlabel` declares its path as
  `/workspaces/:slug/issues/:number/labels/:label`, after its `<label>`
  argument; the hub's route names the segment `:name`. The request is the
  same.
- A `--body` or `--body-file` text that is not valid UTF-8 is a
  `usage_error`: a JSON string cannot carry it byte for byte.
- A close body of white space only is not an empty body: it is `400`
  `invalid_request`.

## The checks use an in-process hub and a fake forge

The package's checks name "a test forge" and "an event recorder in place of
the event log". The tests run against an in-process hub (`hubtest`) whose
forge client's transport trusts `internal/forge/forgetest`, which keeps
in-memory GitHub repositories and GitLab projects for the writes, and wrap
the event emitter through `AdjustForge`. The order of event and response is
observed on test-only copies of the write routes under
`/workspaces/:slug/watched/…`, registered through `hubtest.Config.Compose`.
The fake forge does not model branches, so the second execution path's
"the forge holds main and feature" is not checked; the hub never asks.
