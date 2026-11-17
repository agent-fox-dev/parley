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
// backup ref per branch, holding the last tip replaced. It and
// SwapBackupRef are the one writer of backup refs: WriteBackupRef for a
// reset to origin, SwapBackupRef for a sync that replaces a diverged
// branch. Both run through RunGit.
func WriteBackupRef(ctx context.Context, repo *gitrepo.Repo, branch, id string) error {
	return writeBackupRef(ctx, branch, id, func(ctx context.Context, ref string) error {
		return repo.SetRef(ctx, ref, id)
	})
}

// SwapBackupRef is WriteBackupRef for a writer that read the backup ref
// when it compared the branch, holding was then ("" for none): it points
// the backup ref at id only while the ref still holds was, so that a tip
// another writer saved there meanwhile, and that only the backup ref
// keeps reachable, is not overwritten. Otherwise nothing changes and the
// error matches gitrepo.ErrRefMoved.
func SwapBackupRef(ctx context.Context, repo *gitrepo.Repo, branch, id, was string) error {
	return writeBackupRef(ctx, branch, id, func(ctx context.Context, ref string) error {
		return repo.CompareAndSwapRef(ctx, ref, id, was)
	})
}

// writeBackupRef saves id to the backup ref of branch with write, through
// RunGit.
func writeBackupRef(ctx context.Context, branch, id string, write func(ctx context.Context, ref string) error) error {
	ref := ReplacedRefPrefix + branch
	err := RunGit(ctx, 0, gitrepo.Credential{}, func(ctx context.Context, _ time.Duration) error {
		return write(ctx, ref)
	})
	if err != nil {
		return fmt.Errorf("save %s to %s: %w", id, ref, err)
	}
	return nil
}
