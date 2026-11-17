package carrypatch

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/store"
)

// RerereEntry is a remembered resolution of a carry-patch workspace: a
// conflict git's rerere recorded in a rebuild, with the id git gave it, the
// path of the file it was met at and when the hub recorded it, or learned
// its resolution. Resolved says whether the workspace's repository holds a
// resolution for it, which git replays when a rebuild meets the conflict
// again.
type RerereEntry struct {
	ID, Path   string
	Resolved   bool
	RecordedAt time.Time
}

// rerereSchema holds the remembered-resolution entries, one per conflict
// and workspace. Git keeps the resolutions in the rr-cache of the
// workspace's repository, without the path a conflict was met at; this
// table is the hub's one record of the paths. Whether an entry is resolved
// is read from git, never stored. recorded_at is a UnixNano integer, as
// the other tables keep times.
var rerereSchema = []string{
	`CREATE TABLE rerere_entries (
	workspace_slug TEXT NOT NULL,
	id TEXT NOT NULL,
	path TEXT NOT NULL,
	recorded_at INTEGER NOT NULL,
	PRIMARY KEY (workspace_slug, id)
)`,
}

// RerereSchemaStep adds the remembered-resolution entries. It is a step of
// its own: the store applies a step once, by its name, so the patch lists'
// step, applied already, cannot grow.
var RerereSchemaStep = store.Step{
	Name:   "carry_patch_rerere_entries",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range rerereSchema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	},
}

// RerereEntries lists the remembered-resolution entries of the workspace
// slug, oldest first, each with Resolved read from the workspace's
// repository. An entry git cannot answer for, because there is no copy or
// its rr-cache cannot be read, is listed unresolved rather than failing the
// list: the stored entry is the record, and whether git would replay a
// resolution is what the copy says.
func (c *Component) RerereEntries(ctx context.Context, slug string) ([]RerereEntry, error) {
	if c.d.Store == nil {
		return nil, errNotBound
	}
	rows, err := c.d.Store.Main().QueryContext(ctx,
		`SELECT id, path, recorded_at FROM rerere_entries WHERE workspace_slug = ? ORDER BY recorded_at, id`, slug)
	if err != nil {
		return nil, fmt.Errorf("list the remembered resolutions of %s: %w", slug, err)
	}
	defer func() { _ = rows.Close() }()
	var entries []RerereEntry
	for rows.Next() {
		var e RerereEntry
		var at int64
		if err := rows.Scan(&e.ID, &e.Path, &at); err != nil {
			return nil, fmt.Errorf("list the remembered resolutions of %s: %w", slug, err)
		}
		e.RecordedAt = time.Unix(0, at).UTC()
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the remembered resolutions of %s: %w", slug, err)
	}
	if len(entries) == 0 {
		return entries, nil
	}
	if c.d.RepoPath == nil {
		return entries, nil
	}
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(slug))
	if err != nil {
		return entries, nil
	}
	for i := range entries {
		resolved, err := repo.RerereResolved(entries[i].ID)
		entries[i].Resolved = err == nil && resolved
	}
	return entries, nil
}

// CountRerereEntries is the number of remembered-resolution entries of the
// workspace slug.
func CountRerereEntries(ctx context.Context, q Querier, slug string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT count(*) FROM rerere_entries WHERE workspace_slug = ?`, slug).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the remembered resolutions of %s: %w", slug, err)
	}
	return n, nil
}

// RemoveRerereEntry removes the remembered-resolution entry id of the
// workspace slug in tx, and reports whether there was one. The resolution
// git holds is left to the caller.
func RemoveRerereEntry(ctx context.Context, tx *sql.Tx, slug, id string) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM rerere_entries WHERE workspace_slug = ? AND id = ?`, slug, id)
	if err != nil {
		return false, fmt.Errorf("remove the remembered resolution %s of %s: %w", id, slug, err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RemoveRerereEntries removes, in the transaction tx of a workspace's
// delete, every remembered-resolution entry of the workspace slug (ADR 07).
// A fresh clone of the workspace removes them too, since its copy starts
// with an empty rr-cache.
func RemoveRerereEntries(ctx context.Context, tx *sql.Tx, slug string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM rerere_entries WHERE workspace_slug = ?`, slug); err != nil {
		return fmt.Errorf("remove the remembered resolutions of %s: %w", slug, err)
	}
	return nil
}

// recordRerereEntries records in tx an entry of the workspace slug for each
// of the conflicts git recorded that has none, at the time at. With
// learned, git now holds the conflicts' resolutions, learned at at: an
// entry that exists takes at as its recorded_at.
func recordRerereEntries(ctx context.Context, tx *sql.Tx, slug string, conflicts []gitrepo.RerereConflict, at time.Time, learned bool) error {
	stmt := `INSERT INTO rerere_entries (workspace_slug, id, path, recorded_at) VALUES (?, ?, ?, ?)
	ON CONFLICT (workspace_slug, id) DO NOTHING`
	if learned {
		stmt = `INSERT INTO rerere_entries (workspace_slug, id, path, recorded_at) VALUES (?, ?, ?, ?)
	ON CONFLICT (workspace_slug, id) DO UPDATE SET recorded_at = excluded.recorded_at`
	}
	for _, c := range conflicts {
		if _, err := tx.ExecContext(ctx, stmt, slug, c.ID, c.Path, at.UnixNano()); err != nil {
			return fmt.Errorf("record the remembered resolution %s of %s: %w", c.ID, slug, err)
		}
	}
	return nil
}

// recordConflicts records the conflicts git recorded in the run, learned or
// not as recordRerereEntries says, at the clock's present time.
func (r *rebuildRun) recordConflicts(ctx context.Context, conflicts []gitrepo.RerereConflict, learned bool) error {
	if len(conflicts) == 0 {
		return nil
	}
	if r.c.d.Store == nil {
		return errNotBound
	}
	at := r.c.now().UTC().Truncate(time.Second)
	return r.c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		return recordRerereEntries(ctx, tx, r.ws.Slug, conflicts, at, learned)
	})
}
