package change

import (
	"context"
	"database/sql"
	"fmt"
)

// EventAbandon is the event of a change moved to abandoned.
const EventAbandon = "parley.change.abandon"

// The reasons the hub abandons a change for when its source goes.
const (
	// ReasonSourceDeleted is the reason of a change whose source branch was
	// deleted.
	ReasonSourceDeleted = "source_deleted"
	// ReasonSourceWorkspaceDeleted is the reason of a change whose source
	// workspace, a task fork, was deleted.
	ReasonSourceWorkspaceDeleted = "source_workspace_deleted"
)

// A change outlives its source as a record: abandoning it for a deleted
// source keeps its versions and their refs in the target's repository, and
// only the delete of the workspace it belongs to removes it. These
// functions run in tx, the caller's transaction, and read only the change
// record, so that a delete may call them after the workspace's own record
// is gone.

// SourceDeleted moves the draft or open change whose source is the branch,
// named without refs/heads/, of the workspace (the source workspace, a
// task fork or the change's own workspace) to abandoned for
// ReasonSourceDeleted, as actor did, through Abandon: it writes
// parley.change.abandon, and a fork source's activity recorder is told, for
// the fork lives on and the move may end its work. A branch that is the
// source of no draft or open change changes nothing.
func (c *Component) SourceDeleted(ctx context.Context, tx *sql.Tx, workspace, branch string, actor Caller) error {
	target, number, found, err := activeOfSource(ctx, tx, workspace, branch)
	if err != nil || !found {
		return err
	}
	return c.Abandon(ctx, tx, target, number, ReasonSourceDeleted, actor)
}

// ForkDeleted moves every draft or open change whose source workspace is
// the task fork, all of which belong to its parent, to abandoned for
// ReasonSourceWorkspaceDeleted, as actor did, writing parley.change.abandon
// for each, in number order. Landed and abandoned changes stay as they are.
// The fork activity recorder is not told: the fork is going. A workspace's
// own changes are never touched, so a repository's slug, or one that names
// nothing, changes nothing.
func (c *Component) ForkDeleted(ctx context.Context, tx *sql.Tx, fork string, actor Caller) error {
	rows, err := tx.QueryContext(ctx, `SELECT workspace_slug, number FROM changes
WHERE source_workspace = ? AND workspace_slug <> source_workspace AND state IN ('draft', 'open')
ORDER BY workspace_slug, number`, fork)
	if err != nil {
		return fmt.Errorf("find the changes from %s: %w", fork, err)
	}
	var keys []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.WorkspaceSlug, &k.Number); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, k)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, k := range keys {
		if err := abandonChange(ctx, tx, k.WorkspaceSlug, k.Number, ReasonSourceWorkspaceDeleted, c.now()); err != nil {
			return err
		}
		if err := c.emitAbandon(ctx, tx, actor, k.WorkspaceSlug, k.Number, ReasonSourceWorkspaceDeleted); err != nil {
			return err
		}
	}
	return nil
}

// WorkspaceDeleted deletes every change of the workspace, with its
// versions, session links and landed-commit records, which cascade from
// it, and the workspace's number counter: a workspace created later under
// the same slug is another workspace and numbers its changes from 1. The
// version refs go with the workspace's repository. The changes of other
// workspaces stay, also those whose source the workspace is, which
// ForkDeleted abandons.
func (c *Component) WorkspaceDeleted(ctx context.Context, tx *sql.Tx, workspace string) error {
	for _, stmt := range []string{
		`DELETE FROM changes WHERE workspace_slug = ?`,
		`DELETE FROM change_counters WHERE workspace_slug = ?`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, workspace); err != nil {
			return fmt.Errorf("delete the changes of %s: %w", workspace, err)
		}
	}
	return nil
}

// emitAbandon writes parley.change.abandon for the change number of the
// workspace, abandoned by actor for reason, with its current version and
// head.
func (c *Component) emitAbandon(ctx context.Context, tx *sql.Tx, actor Caller, workspace string, number int64, reason string) error {
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil {
		return err
	}
	return c.emit(ctx, tx, actor, ch, EventAbandon, map[string]any{"reason": reason, "version": ch.Version, "head_sha": ch.HeadSHA})
}
