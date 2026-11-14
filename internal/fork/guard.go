package fork

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// Use is what a caller wants of a fork: to read it or to write to it.
type Use int

// The uses of CheckParentState.
const (
	UseRead Use = iota
	UseWrite
)

// CheckParentState is the check every route that reads or writes a fork
// makes of the fork's parent: nil when the workspace slug is a repository
// workspace or a fork whose parent is ready, and otherwise the 409 the route
// answers unchanged. A fork follows its parent: while the parent is busy (a
// maintenance operation runs on it, or it is pending or cloning) both uses
// are 409 workspace_busy, and while its clone has failed 409
// workspace_not_ready; while it is archived the fork stays readable and a
// write is 409 parent_archived. A slug no workspace has is 404 workspace not
// found.
func (c *Component) CheckParentState(ctx context.Context, slug string, use Use) error {
	w, found, err := workspace.BySlug(ctx, c.deps.Store.Main(), slug)
	switch {
	case err != nil:
		return err
	case !found:
		return server.NotFound(workspace.ErrNotFound.Error())
	}
	return parentState(ctx, c.deps.Store.Main(), w, use)
}

// GuardUpdate is the guard of every update of a fork, of whichever field,
// a workspace.ForkUpdateGuard: the parent-state check for a write, with the
// parent read through tx, the update's transaction, so that the update is
// judged against the parent as the transaction sees it.
func GuardUpdate(ctx context.Context, tx *sql.Tx, f workspace.Workspace) error {
	return parentState(ctx, tx, f, UseWrite)
}

// parentState is CheckParentState of the workspace w, its parent read
// through q.
func parentState(ctx context.Context, q access.Querier, w workspace.Workspace, use Use) error {
	if w.Kind != access.KindFork || w.ParentSlug == nil {
		return nil
	}
	parent, found, err := workspace.BySlug(ctx, q, *w.ParentSlug)
	switch {
	case err != nil:
		return err
	case !found:
		return fmt.Errorf("fork %q: its parent %q has no record", w.Slug, *w.ParentSlug)
	}
	// A maintenance operation on an archived parent (its delete) is work in
	// progress, which RequireReady answers as busy, for both uses.
	if parent.Status == workspace.StatusArchived && parent.Marker == workspace.MarkerNone {
		if use == UseRead {
			return nil
		}
		return server.NewError("parent_archived", http.StatusConflict,
			fmt.Sprintf("workspace %q is read-only: its parent workspace %q is archived", w.Slug, parent.Slug))
	}
	return workspace.RequireReady(parent)
}
