package carrypatch

import (
	"context"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// WriteBackupRef points the backup ref of branch in repo,
// refs/parley/replaced/<branch>, at the commit id, the tip the hub is about
// to discard, overwriting the tip an earlier replace saved there: one
// backup ref per branch, holding the last tip replaced. It is the one
// writer of backup refs, for a reset to origin and for a sync that
// replaces a diverged branch alike, and runs through RunGit.
func WriteBackupRef(ctx context.Context, repo *gitrepo.Repo, branch, id string) error {
	ref := ReplacedRefPrefix + branch
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return repo.SetRef(ctx, ref, id)
	})
	if err != nil {
		return fmt.Errorf("save %s to %s: %w", id, ref, err)
	}
	return nil
}
