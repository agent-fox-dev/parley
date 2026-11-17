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

// purgeRefLimit is the deadline of each git the purge runs on a backup
// ref. The deletion runs inside the patch's store transaction, and every
// write of the hub waits on that transaction: a git that hangs must not
// hold them for DefaultGitDeadline.
const purgeRefLimit = 30 * time.Second

// purgePatch removes the patch p, chosen as deleted before cutoff, in one
// store transaction: the row, only while it is still deleted before
// cutoff, so that a restore that committed first makes it remove nothing,
// then, when no patch of the workspace that is not deleted uses its branch,
// the branch's backup ref in the copy. A failure of either rolls the
// transaction back, so the next pass tries both again. The ref is deleted
// before the commit, so a transaction that fails after the deletion would
// leave the row without its ref: the ref is then set back to the commit it
// held. A workspace without a copy has no backup ref to delete.
func (c *Component) purgePatch(ctx context.Context, p Patch, cutoff time.Time) error {
	var repo *gitrepo.Repo
	var deleted gitrepo.Ref
	err := c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
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
		repo, err = c.d.Git.Open(ctx, c.d.RepoPath(p.Workspace))
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("open the copy: %w", err)
		}
		ref := ReplacedRefPrefix + p.BranchName
		if err := RunGit(ctx, purgeRefLimit, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
			old, found, err := repo.LookupRef(ctx, ref)
			if err != nil || !found {
				return err
			}
			if err := repo.DeleteRef(ctx, ref); err != nil {
				return err
			}
			deleted = old
			return nil
		}); err != nil {
			return fmt.Errorf("delete %s: %w", ref, err)
		}
		if deleted.Name != "" && c.d.AfterPurgeRef != nil {
			return c.d.AfterPurgeRef(p.Workspace, p.ID)
		}
		return nil
	})
	if err != nil && deleted.Name != "" {
		c.restorePurgedRef(ctx, repo, p, deleted)
	}
	return err
}

// restorePurgedRef sets the backup ref ref of the patch p back to the
// commit it held before a purge whose transaction failed deleted it. It
// creates the ref only while it is absent, so that a value another writer
// gave it since is kept, and runs whether or not the caller left: the row
// is kept, and its ref goes with it. A failure is logged.
func (c *Component) restorePurgedRef(ctx context.Context, repo *gitrepo.Repo, p Patch, ref gitrepo.Ref) {
	ctx = context.WithoutCancel(ctx)
	err := RunGit(ctx, purgeRefLimit, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.CompareAndSwapRef(ctx, ref.Name, ref.ID, "")
	})
	if err != nil {
		c.logger().LogAttrs(ctx, slog.LevelError, "backup ref of a patch not purged not restored",
			slog.String("workspace", p.Workspace), slog.String("patch_id", p.ID),
			slog.String("ref", ref.Name), slog.String("commit", ref.ID), slog.String("error", err.Error()))
	}
}
