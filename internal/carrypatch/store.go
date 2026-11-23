package carrypatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/agent-fox-dev/parley/internal/store"
)

// schema holds the patch lists: one row per patch, keyed by its UUID, under
// the workspace's slug. A deleted patch keeps its row, without a position,
// until it is purged, so a branch is unique only among the patches that are
// not deleted. conflict_files is a JSON array. Times are UnixNano integers,
// as the other tables keep them. Positions carry no unique index: moving
// the patches at and after a position down is one UPDATE, which a unique
// index would refuse halfway.
var schema = []string{
	`CREATE TABLE patches (
	id TEXT PRIMARY KEY,
	workspace_slug TEXT NOT NULL,
	branch_name TEXT NOT NULL,
	position INTEGER CHECK (position >= 1),
	status TEXT NOT NULL CHECK (status IN ('active', 'conflict', 'disabled', 'merged_upstream', 'deleted')),
	conflict_files TEXT NOT NULL DEFAULT '[]',
	upstream_pr_url TEXT,
	description TEXT,
	added_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	deleted_at INTEGER,
	origin_sync_state TEXT,
	origin_sha TEXT,
	origin_synced_at INTEGER,
	CHECK ((status = 'deleted') = (position IS NULL))
)`,
	`CREATE UNIQUE INDEX patches_live_branch ON patches (workspace_slug, branch_name) WHERE status <> 'deleted'`,
	`CREATE INDEX patches_position ON patches (workspace_slug, position)`,
	`CREATE INDEX patches_deleted ON patches (workspace_slug, deleted_at)`,
}

// SchemaStep adds the patch lists.
var SchemaStep = store.Step{
	Name:   "carry_patch_patches",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		for _, stmt := range schema {
			if _, err := tx.ExecContext(ctx, stmt); err != nil {
				return err
			}
		}
		return nil
	},
}

// Querier is what a read goes through: the store's *sql.DB or the caller's
// open *sql.Tx, so that a read inside a transaction sees what it wrote.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ErrPatchExists is the error of InsertPatch for a branch that a patch of
// the workspace that is not deleted has already.
var ErrPatchExists = errors.New("the branch is a patch of the workspace already")

// errNotBound is the error of an operation that needs the store before Bind.
var errNotBound = errors.New("the carry-patch component is not bound")

// patchColumns are the columns scanPatch reads, in its order.
const patchColumns = `id, workspace_slug, branch_name, position, status, conflict_files, upstream_pr_url, description,
	added_at, updated_at, deleted_at, origin_sync_state, origin_sha, origin_synced_at`

// InsertPatch stores p in tx. A branch that a patch of the workspace that is
// not deleted has already is an error matching ErrPatchExists, and writes
// nothing.
func InsertPatch(ctx context.Context, tx *sql.Tx, p Patch) error {
	files := p.ConflictFiles
	if files == nil {
		files = []string{}
	}
	encoded, err := json.Marshal(files)
	if err != nil {
		return fmt.Errorf("insert the patch of %s in %s: %w", p.BranchName, p.Workspace, err)
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO patches (`+patchColumns+`)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID, p.Workspace, p.BranchName, p.Position, p.Status, string(encoded), p.UpstreamPRURL, p.Description,
		p.AddedAt.UnixNano(), p.UpdatedAt.UnixNano(), store.Nanos(p.DeletedAt), p.OriginSyncState, p.OriginSHA, store.Nanos(p.OriginSyncedAt))
	if isDuplicateBranch(err) {
		return fmt.Errorf("insert the patch of %s in %s: %w", p.BranchName, p.Workspace, ErrPatchExists)
	}
	if err != nil {
		return fmt.Errorf("insert the patch of %s in %s: %w", p.BranchName, p.Workspace, err)
	}
	return nil
}

// ReadPatch reads the patch id of the workspace, deleted or not; found is
// false when the workspace has no such patch.
func ReadPatch(ctx context.Context, q Querier, workspace, id string) (p Patch, found bool, err error) {
	return onePatch(ctx, q, `WHERE workspace_slug = ? AND id = ?`, workspace, id)
}

// LivePatchOfBranch reads the patch of branch in the workspace that is not
// deleted; found is false when there is none.
func LivePatchOfBranch(ctx context.Context, q Querier, workspace, branch string) (p Patch, found bool, err error) {
	return onePatch(ctx, q, `WHERE workspace_slug = ? AND branch_name = ? AND status <> 'deleted'`, workspace, branch)
}

// DeletedPatchOfBranch reads the deleted patch of branch in the workspace,
// the one deleted last when there are several; found is false when there is
// none.
func DeletedPatchOfBranch(ctx context.Context, q Querier, workspace, branch string) (p Patch, found bool, err error) {
	return onePatch(ctx, q, `WHERE workspace_slug = ? AND branch_name = ? AND status = 'deleted'
ORDER BY deleted_at DESC, id LIMIT 1`, workspace, branch)
}

// ListLivePatches lists the patches of the workspace that are not deleted,
// in position order.
func ListLivePatches(ctx context.Context, q Querier, workspace string) ([]Patch, error) {
	return listPatches(ctx, q, `WHERE workspace_slug = ? AND status <> 'deleted' ORDER BY position`, workspace)
}

// ListDeletedPatches lists the deleted patches of the workspace, the one
// deleted last first, ties by id.
func ListDeletedPatches(ctx context.Context, q Querier, workspace string) ([]Patch, error) {
	return listPatches(ctx, q, `WHERE workspace_slug = ? AND status = 'deleted' ORDER BY deleted_at DESC, id`, workspace)
}

// ShiftPositions moves every patch of the workspace that is not deleted and
// stands at position from or after it down by one, making room at from.
func ShiftPositions(ctx context.Context, tx *sql.Tx, workspace string, from int) error {
	if _, err := tx.ExecContext(ctx, `UPDATE patches SET position = position + 1
WHERE workspace_slug = ? AND status <> 'deleted' AND position >= ?`, workspace, from); err != nil {
		return fmt.Errorf("move the patches of %s from position %d: %w", workspace, from, err)
	}
	return nil
}

// CountLivePatches counts the patches of the workspace that are not deleted.
func CountLivePatches(ctx context.Context, q Querier, workspace string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM patches WHERE workspace_slug = ? AND status <> 'deleted'`,
		workspace).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the patches of %s: %w", workspace, err)
	}
	return n, nil
}

// countApplicable counts the patches of the workspace a rebuild applies:
// those in active or conflict.
func countApplicable(ctx context.Context, q Querier, workspace string) (int, error) {
	var n int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM patches WHERE workspace_slug = ? AND status IN (?, ?)`,
		workspace, StatusActive, StatusConflict).Scan(&n); err != nil {
		return 0, fmt.Errorf("count the patches of %s to rebuild: %w", workspace, err)
	}
	return n, nil
}

// DeletePatch removes the row of the patch id of the workspace for good; a
// patch that does not exist is no error.
func DeletePatch(ctx context.Context, tx *sql.Tx, workspace, id string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM patches WHERE workspace_slug = ? AND id = ?`, workspace, id); err != nil {
		return fmt.Errorf("remove the patch %s of %s: %w", id, workspace, err)
	}
	return nil
}

// RemoveWorkspace removes, in the transaction tx of a workspace's delete,
// every patch of the workspace slug, deleted or not (ADR 07).
func RemoveWorkspace(ctx context.Context, tx *sql.Tx, slug string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM patches WHERE workspace_slug = ?`, slug); err != nil {
		return fmt.Errorf("remove the patches of %s: %w", slug, err)
	}
	return nil
}

// IsPatchBranch reports whether branch is the branch of a patch of the
// workspace slug that is not deleted, so that the push route leaves the
// patch branches to the patch list.
func (c *Component) IsPatchBranch(ctx context.Context, slug, branch string) (bool, error) {
	if c.d.Store == nil {
		return false, errNotBound
	}
	_, found, err := LivePatchOfBranch(ctx, c.d.Store.Main(), slug, branch)
	return found, err
}

// onePatch reads the first patch the query's tail where selects.
func onePatch(ctx context.Context, q Querier, where string, args ...any) (Patch, bool, error) {
	p, err := scanPatch(q.QueryRowContext(ctx, `SELECT `+patchColumns+` FROM patches `+where, args...).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return Patch{}, false, nil
	}
	if err != nil {
		return Patch{}, false, fmt.Errorf("read a patch: %w", err)
	}
	return p, true, nil
}

// listPatches reads every patch the query's tail where selects, in its
// order.
func listPatches(ctx context.Context, q Querier, where string, args ...any) ([]Patch, error) {
	rows, err := q.QueryContext(ctx, `SELECT `+patchColumns+` FROM patches `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("list patches: %w", err)
	}
	defer rows.Close()
	var out []Patch
	for rows.Next() {
		p, err := scanPatch(rows.Scan)
		if err != nil {
			return nil, fmt.Errorf("list patches: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list patches: %w", err)
	}
	return out, nil
}

// scanPatch reads one row of patchColumns.
func scanPatch(scan func(dest ...any) error) (Patch, error) {
	var p Patch
	var position sql.NullInt64
	var files string
	var added, updated int64
	var deleted, synced sql.NullInt64
	if err := scan(&p.ID, &p.Workspace, &p.BranchName, &position, &p.Status, &files, &p.UpstreamPRURL, &p.Description,
		&added, &updated, &deleted, &p.OriginSyncState, &p.OriginSHA, &synced); err != nil {
		return Patch{}, err
	}
	if position.Valid {
		n := int(position.Int64)
		p.Position = &n
	}
	if err := json.Unmarshal([]byte(files), &p.ConflictFiles); err != nil {
		return Patch{}, fmt.Errorf("the conflict files of the patch %s: %w", p.ID, err)
	}
	p.AddedAt, p.UpdatedAt = time.Unix(0, added).UTC(), time.Unix(0, updated).UTC()
	p.DeletedAt, p.OriginSyncedAt = store.TimeOf(deleted), store.TimeOf(synced)
	return p, nil
}

// isDuplicateBranch reports whether err is SQLite refusing a duplicate of
// the one unique index of the patches, that of a branch among the patches
// of a workspace that are not deleted; a duplicate id is a primary key
// violation and not this.
func isDuplicateBranch(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE
}
