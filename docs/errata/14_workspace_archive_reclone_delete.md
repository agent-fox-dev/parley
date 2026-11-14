# Errata: 14_workspace_archive_reclone_delete

Where the implementation of package 14
(`.specs/14_workspace_archive_reclone_delete`) differs from what the
package says, or decides what it leaves open.

## The maintenance marker is stored in the workspace record

The package (decision 4) keeps the maintenance marker in memory, one entry
per slug in the `parleyd` process. Package 10 had already given the marker
a place before this package was built: the column `maintenance` of the
`workspaces` table (`''`, `reclone`, `archive` or `delete`), which
`workspace.BySlug` reads into `Workspace.Marker` and the state guard
`RequireReady` reads from the record it is given. The marker is therefore
that column, and no process-wide map is added beside it.

- Setting the marker is one conditional write,
  `UPDATE workspaces SET maintenance = ? WHERE slug = ? AND maintenance = ''`:
  of two operations that ask at once exactly one changes the row and holds
  the marker, without a lock in the process. The other is answered by
  `RequireReady` on the record read again (`409 workspace_busy`).
- `(*workspace.Service).withMarker` runs the operation with the marker held
  and releases it in a deferred write, on a context the request's end does
  not cancel and only while the column still holds its own marker, so that
  it is released after a success, a refusal, a failure and a panic.
- Neither write changes `updated_at`: the marker is the hub's own and no
  response shows it.
- Decision 4 promises that a hub that stops during an operation starts with
  no marker. A stored marker would outlive the process, so the start hook
  `workspaces` in `internal/hub/workspace.go` releases every marker with
  `workspace.ClearMarkers` before the clone workers queue their work and
  before the hub serves. Each operation changes the store in one
  transaction, so a marker left this way guards nothing.
