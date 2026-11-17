package carrypatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// PurgeAge is how long a deleted patch is kept, for a restore, before the
// purge removes it for good.
const PurgeAge = 7 * 24 * time.Hour

// PurgeDeletedPatches removes for good, in every workspace whatever its
// status, each patch that was deleted more than PurgeAge before now; one
// deleted exactly PurgeAge before is kept. Each patch goes in a store
// transaction of its own (purgePatch), so a restore that commits first
// keeps its patch, and a patch whose removal fails stays as it was while
// the others go. A failure is logged with the workspace's slug and the
// patch's id, and the pass goes on; the error returned at the end names
// every patch that stayed, so that the maintenance loop reports the task
// as failed. The patch branch is kept, no position changes, since a
// deleted patch has none, and no event is written.
func (c *Component) PurgeDeletedPatches(ctx context.Context, now time.Time) error {
	if c.d.Store == nil {
		return errNotBound
	}
	cutoff := now.Add(-PurgeAge)
	due, err := listPatches(ctx, c.d.Store.Main(), `WHERE status = ? AND deleted_at < ? ORDER BY workspace_slug, deleted_at, id`,
		StatusDeleted, cutoff.UnixNano())
	if err != nil {
		return fmt.Errorf("list the deleted patches due for purge: %w", err)
	}
	var failed []error
	for _, p := range due {
		if err := ctx.Err(); err != nil {
			return err
		}
		if c.d.BeforePurgePatch != nil {
			c.d.BeforePurgePatch(p.Workspace, p.ID)
		}
		if err := c.purgePatch(ctx, p, cutoff); err != nil {
			c.logger().LogAttrs(ctx, slog.LevelError, "deleted patch not purged",
				slog.String("workspace", p.Workspace), slog.String("patch_id", p.ID),
				slog.String("branch", p.BranchName), slog.String("error", err.Error()))
			failed = append(failed, fmt.Errorf("the patch %s of %s: %w", p.ID, p.Workspace, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d deleted patches not purged: %w", len(failed), len(due), errors.Join(failed...))
	}
	return nil
}

// purgePatch removes the patch p, chosen as deleted before cutoff, in one
// store transaction: the row, only while it is still deleted before
// cutoff, so that a restore that committed first makes it remove nothing,
// then, when no patch of the workspace that is not deleted uses its branch,
// the branch's backup ref in the copy. A failure of either rolls the
// transaction back, so the next pass tries both again. A workspace without
// a copy has no backup ref to delete.
func (c *Component) purgePatch(ctx context.Context, p Patch, cutoff time.Time) error {
	return c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `DELETE FROM patches WHERE workspace_slug = ? AND id = ? AND status = ? AND deleted_at < ?`,
			p.Workspace, p.ID, StatusDeleted, cutoff.UnixNano())
		if err != nil {
			return fmt.Errorf("delete the patch: %w", err)
		}
		if n, err := res.RowsAffected(); err != nil {
			return fmt.Errorf("delete the patch: %w", err)
		} else if n == 0 {
			return nil
		}
		if _, held, err := LivePatchOfBranch(ctx, tx, p.Workspace, p.BranchName); err != nil {
			return err
		} else if held {
			return nil
		}
		repo, err := c.d.Git.Open(ctx, c.d.RepoPath(p.Workspace))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("open the copy: %w", err)
		}
		ref := ReplacedRefPrefix + p.BranchName
		if err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
			return repo.DeleteRef(ctx, ref)
		}); err != nil {
			return fmt.Errorf("delete %s: %w", ref, err)
		}
		return nil
	})
}
