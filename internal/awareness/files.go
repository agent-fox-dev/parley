package awareness

import (
	"context"
	"slices"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// versionFiles are the files a version touches: every file that differs
// between base (nil for the empty tree) and head, renames and copies
// found, a renamed or copied file under both its old and its new path,
// ascending and without a cap, as the change component counts a change's
// paths.
func versionFiles(ctx context.Context, repo *gitrepo.Repo, base *string, head string) ([]string, error) {
	from := ""
	if base != nil {
		from = *base
	}
	diff, err := repo.DiffChanges(ctx, from, head)
	if err != nil {
		return nil, err
	}
	paths := make([]string, 0, len(diff))
	for _, ch := range diff {
		paths = append(paths, ch.Path)
		if ch.OldPath != "" {
			paths = append(paths, ch.OldPath)
		}
	}
	slices.Sort(paths)
	return slices.Compact(paths), nil
}

// listFiles are the files of ch's current version in the repository of
// its workspace, where the version's commits are.
func (c *Component) listFiles(ctx context.Context, ch Change) ([]string, error) {
	repo, err := c.deps.Repos(ctx, ch.Workspace)
	if err != nil {
		return nil, err
	}
	return versionFiles(ctx, repo, ch.BaseSHA, ch.HeadSHA)
}
