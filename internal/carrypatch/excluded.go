package carrypatch

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/workspace"
)

// The component names the patch branches that the per-branch pass of a
// sync leaves out.
var _ workspace.ExcludedBranchProvider = (*Component)(nil)

// ExcludedBranches names the branch of every patch of the workspace slug
// that is not deleted, in patch-list order: those branches are moved by the
// origin refresh, the rebuild and the patch routes, never by the general
// per-branch pass or its reset to origin. A deleted patch's branch is an
// ordinary branch again. A patch list that cannot be read is an error and
// no names, never a partial list, so that the sync stops before it compares
// a patch branch it would otherwise treat as ordinary.
func (c *Component) ExcludedBranches(ctx context.Context, slug string) ([]string, error) {
	if c.d.Store == nil {
		return nil, errNotBound
	}
	live, err := ListLivePatches(ctx, c.d.Store.Main(), slug)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(live))
	for _, p := range live {
		names = append(names, p.BranchName)
	}
	return names, nil
}
