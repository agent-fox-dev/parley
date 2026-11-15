package change

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
)

// The setters store, in tx, the caller's transaction, a field of the
// change number of the workspace that another feature computes, and set
// its updated_at; a change that does not exist is the change-not-found
// error, and nothing is stored. The change's dependants are not stored:
// they are read from the depends_on of the other changes.

// SetDependsOn stores the change the change depends on, nil for none.
func (c *Component) SetDependsOn(ctx context.Context, tx *sql.Tx, workspace string, number int64, dependsOn *int64) error {
	return update(ctx, tx, workspace, number, c.now(), `depends_on = ?`, dependsOn)
}

// SetBase makes base the base of the change's current version and
// measures the version against it again: its base_sha, stats, touched
// paths and patch id, on the change and on the version. The measuring runs
// in the change's own repository, which holds the head under its version
// ref, also for a fork's change, and the new base, such as the head of the
// change it now depends on or of the branch it now targets.
func (c *Component) SetBase(ctx context.Context, tx *sql.Tx, workspace string, number int64, base string) error {
	ch, err := readChange(ctx, tx, workspace, number)
	if err != nil {
		return err
	}
	o := &opening{head: ch.HeadSHA, base: base}
	if o.sourceRepo, err = c.deps.Repos(ctx, workspace); err != nil {
		return err
	}
	ahead, err := o.sourceRepo.CommitsBetween(ctx, base, ch.HeadSHA, 0)
	if err != nil {
		return err
	}
	if err := o.measureFromBase(ctx, len(ahead)); err != nil {
		return err
	}
	return storeBase(ctx, tx, workspace, number, ch.Version, o, c.now())
}

// SetMergeable stores whether the change merges cleanly into its target,
// one of MergeableClean, MergeableConflict and MergeableUnknown, with the
// conflict report, which is kept only for MergeableConflict.
func (c *Component) SetMergeable(ctx context.Context, tx *sql.Tx, workspace string, number int64, mergeable string, conflict json.RawMessage) error {
	var report *string
	switch mergeable {
	case MergeableConflict:
		if len(conflict) > 0 {
			report = new(string(conflict))
		}
	case MergeableClean, MergeableUnknown:
	default:
		return fmt.Errorf("set mergeable of %s: %q is not a value of mergeable", Ref(workspace, number), mergeable)
	}
	return update(ctx, tx, workspace, number, c.now(), `mergeable = ?, conflict = ?`, mergeable, report)
}

// SetNeedsRestack stores whether the change needs a restack, and why; the
// reason is dropped when it does not.
func (c *Component) SetNeedsRestack(ctx context.Context, tx *sql.Tx, workspace string, number int64, needs bool, reason *string) error {
	if !needs {
		reason = nil
	}
	return update(ctx, tx, workspace, number, c.now(), `needs_restack = ?, restack_reason = ?`, needs, reason)
}

// SetLanding stores the change's landing.
func (c *Component) SetLanding(ctx context.Context, tx *sql.Tx, workspace string, number int64, l Landing) error {
	return storeLanding(ctx, tx, workspace, number, l, c.now())
}

// SetLanded moves the change to landed, storing how it landed and its
// landed-commit records, l.Commits, in place of any it had. The fork
// activity recorder is not told: a landing reaches the fork feature
// through the landing's own hook.
func (c *Component) SetLanded(ctx context.Context, tx *sql.Tx, workspace string, number int64, l Landed) error {
	return landChange(ctx, tx, workspace, number, l, c.now())
}

// SetReverts stores the change the change reverts, nil for none.
func (c *Component) SetReverts(ctx context.Context, tx *sql.Tx, workspace string, number int64, reverts *int64) error {
	return update(ctx, tx, workspace, number, c.now(), `reverts = ?`, reverts)
}

// SetRevertedBy stores the change that reverts the change, nil for none.
func (c *Component) SetRevertedBy(ctx context.Context, tx *sql.Tx, workspace string, number int64, by *int64) error {
	return update(ctx, tx, workspace, number, c.now(), `reverted_by = ?`, by)
}

// SetAbandoned moves the change to abandoned for reason and, when its
// source is a task fork, tells the fork activity recorder, whose error is
// logged and changes nothing.
func (c *Component) SetAbandoned(ctx context.Context, tx *sql.Tx, workspace string, number int64, reason string) error {
	if err := abandonChange(ctx, tx, workspace, number, reason, c.now()); err != nil {
		return err
	}
	activity := c.collaborators().activity
	if activity == nil {
		return nil
	}
	var source string
	if err := tx.QueryRowContext(ctx, `SELECT source_workspace FROM changes WHERE workspace_slug = ? AND number = ?`,
		workspace, number).Scan(&source); err != nil {
		return fmt.Errorf("read the source of %s: %w", Ref(workspace, number), err)
	}
	// A change's source is its own workspace or a fork of it.
	if source != workspace {
		if err := activity.RecordActivity(ctx, tx, source); err != nil {
			c.deps.Logger.Warn("the fork's activity was not recorded", "fork", source, "change", Ref(workspace, number), "error", err)
		}
	}
	return nil
}
