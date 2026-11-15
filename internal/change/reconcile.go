package change

import (
	"context"
	"database/sql"
	"fmt"
)

// Reconcile brings every draft and open change in line with its source
// branch, as a start of the hub must before it accepts a request: a stop
// between the move of a branch and the transaction that records it loses
// the bookkeeping, not the move, and mergeability checks are not kept
// across a stop.
//
// Each change is reconciled in a store transaction of its own: a source
// branch at the head of the change's current version changes nothing; one
// at another commit records that commit as the next version, of kind ref,
// by the hub with no credential, with everything a new version brings; a
// missing source branch of a source workspace that still has its record
// abandons the change for ReasonSourceDeleted, by the hub. A change that
// cannot be reconciled (a repository that cannot be read, a version that
// cannot be recorded, or a source workspace without a record, whose
// changes the delete of the workspace settles) is logged with its
// workspace and number and left as it is, and the others are reconciled
// all the same. Then every draft and open change whose mergeable is
// unknown is scheduled for a check, which the worker runs once it starts.
//
// Only a failure to list the changes is returned.
func (c *Component) Reconcile(ctx context.Context) error {
	keys, err := c.activeChanges(ctx, "")
	if err != nil {
		return err
	}
	for _, k := range keys {
		if err := ctx.Err(); err != nil {
			return err
		}
		var abandoned bool
		err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) (err error) {
			abandoned, err = c.reconcile(ctx, tx, k)
			return err
		})
		switch {
		case err != nil:
			c.deps.Logger.Error("the change was not reconciled with its source branch", "workspace", k.WorkspaceSlug,
				"number", k.Number, "error", err)
		case abandoned:
			c.merge.drop(mergeKey{k.WorkspaceSlug, k.Number})
		}
	}
	unknown, err := c.activeChanges(ctx, " AND mergeable = 'unknown'")
	if err != nil {
		return err
	}
	for _, k := range unknown {
		c.merge.pend(mergeKey{k.WorkspaceSlug, k.Number})
	}
	return nil
}

// reconcile brings the change k in line with its source branch in tx, and
// reports whether it abandoned the change.
func (c *Component) reconcile(ctx context.Context, tx *sql.Tx, k Key) (abandoned bool, err error) {
	ch, err := readChange(ctx, tx, k.WorkspaceSlug, k.Number)
	if err != nil || (ch.State != StateDraft && ch.State != StateOpen) {
		return false, err
	}
	source := ch.Source.WorkspaceSlug
	if _, found, err := c.deps.Workspaces.Workspace(ctx, tx, source); err != nil || !found {
		if err == nil {
			err = fmt.Errorf("its source workspace %s has no record", source)
		}
		return false, err
	}
	repo, err := c.deps.Repos(ctx, source)
	if err != nil {
		return false, err
	}
	head, found, err := repo.ResolveBranch(ctx, ch.Source.Branch)
	switch {
	case err != nil:
		return false, err
	case !found:
		return true, c.Abandon(ctx, tx, k.WorkspaceSlug, k.Number, ReasonSourceDeleted, Caller{})
	case head == ch.HeadSHA:
		return false, nil
	}
	v, err := c.RecordVersion(ctx, tx, VersionInput{Workspace: source, Branch: ch.Source.Branch, Head: head, Kind: KindRef})
	if err == nil && v == nil {
		err = fmt.Errorf("no version was recorded for %s at %s", ch.Source.Branch, head)
	}
	return false, err
}

// activeChanges are the draft and open changes that also meet the SQL
// condition and, which starts with AND or is empty, in workspace and number
// order.
func (c *Component) activeChanges(ctx context.Context, and string) ([]Key, error) {
	rows, err := c.deps.Store.Main().QueryContext(ctx, `SELECT workspace_slug, number FROM changes
WHERE state IN ('draft', 'open')`+and+` ORDER BY workspace_slug, number`)
	if err != nil {
		return nil, fmt.Errorf("list the draft and open changes: %w", err)
	}
	defer rows.Close()
	var keys []Key
	for rows.Next() {
		var k Key
		if err := rows.Scan(&k.WorkspaceSlug, &k.Number); err != nil {
			return nil, err
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
