package change

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// ReadVersions is the read of a change's versions, which the tests use
// before the routes that serve them.
var ReadVersions = readVersions

// PatchID is the patch id the component stores for a version from base to
// head in repo.
func PatchID(ctx context.Context, repo *gitrepo.Repo, base, head string) (string, error) {
	changes, err := repo.DiffChanges(ctx, base, head)
	if err != nil {
		return "", err
	}
	files, err := keyed(ctx, repo, base, head, changes)
	return patchID(files), err
}
