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

## The marker waits out work in flight, and a delete holds the copies' places

The package makes the marker turn other requests away (REQ-1.1). It does not
say what happens to work that passed the state guard just before the marker
was set, or to a workspace created again with a deleted slug while the delete
is still removing the old copy (REQ-6.6). The implementation settles both:

- Every sync and push of a workspace holds the workspace's entry of
  `Service.work`, a readers-writer mutex per slug, shared for its whole run
  (`admitWork`). It judges the record once before taking the entry and once
  under it. `withMarker` takes the entry alone once the marker is set, so an
  archive, a reclone and a delete first wait for that work to end, and a
  request that waited for the operation is refused on the record the
  operation left.
- A delete takes the `Service.copies` entries of the workspace and its forks
  before its transaction and holds them until the copies are removed. Clone
  work holds its slug's entry while it runs, so the clone of a workspace
  created again with the slug starts only after the old copy is gone.
- A delete lists the forks before it removes their sandboxes. A fork its
  transaction would remove that is not in that list was made while the
  delete ran, and its sandbox may still run, so the transaction rolls back
  with `409 workspace_busy` and the delete can be repeated.
