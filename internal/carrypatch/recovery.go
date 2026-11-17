package carrypatch

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// DiscardRebuildLeftovers discards, in the copy of every carry-patch
// workspace, what a rebuild interrupted by a stop or a crash of the hub
// left there: every entry of the copy's scratch working tree directory and
// the copy's record of every working tree whose directory is gone
// ((*gitrepo.Repo).DiscardScratches), through RunGit. A rebuild makes no
// ref for a run, so no ref is deleted, and no ref a client reads is
// touched.
//
// It is for the hub's start, before the job workers run: no rebuild works
// in a copy then. A workspace whose clone work is outstanding (pending,
// cloning) is left out, since its clone replaces the copy. A copy that is
// missing or whose leftovers cannot be removed is logged with the
// workspace's slug and the next workspace goes on, so that no leftover
// stops the hub from starting; only a failure to read the workspaces is
// returned.
func (c *Component) DiscardRebuildLeftovers(ctx context.Context) error {
	if c.d.Store == nil {
		return errNotBound
	}
	slugs, err := carryPatchCopies(ctx, c.d.Store.Main())
	if err != nil {
		return err
	}
	for _, slug := range slugs {
		if err := c.discardLeftovers(ctx, slug); err != nil {
			c.logger().Warn("leftovers of an interrupted rebuild not discarded", "workspace", slug, "error", err)
		}
	}
	return nil
}

// discardLeftovers discards the leftovers of an interrupted rebuild in the
// copy of the workspace slug.
func (c *Component) discardLeftovers(ctx context.Context, slug string) error {
	repo, err := c.d.Git.Open(ctx, c.d.RepoPath(slug))
	if err != nil {
		return fmt.Errorf("open the copy: %w", err)
	}
	return RunGit(ctx, c.d.RebuildGitDeadline, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.DiscardScratches(ctx)
	})
}

// carryPatchCopies lists through q, in slug order, the slugs of the
// carry-patch workspaces that have or should have a copy: those whose clone
// work is not outstanding.
func carryPatchCopies(ctx context.Context, q Querier) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT slug FROM workspaces WHERE workspace_mode = ? AND status NOT IN (?, ?) ORDER BY slug`,
		workspace.ModeCarryPatch, string(workspace.StatusPending), string(workspace.StatusCloning))
	if err != nil {
		return nil, fmt.Errorf("list the carry-patch workspaces: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("list the carry-patch workspaces: %w", err)
		}
		slugs = append(slugs, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the carry-patch workspaces: %w", err)
	}
	return slugs, nil
}
