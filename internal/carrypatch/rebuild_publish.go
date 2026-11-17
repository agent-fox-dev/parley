package carrypatch

import (
	"context"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
)

// publish moves the integration branch named on the job to the head under
// construction in one compare-and-swap against the head it reads just
// before, creating the branch when it does not exist, and stores both
// heads on the job; with no patch applied, the head is the upstream base.
// It is the one ref the run moves. Another writer that moves the branch in
// between makes the swap fail, and the attempt with it, and a workspace
// that is no longer ready just before the swap ends the attempt as at its
// start.
func (r *rebuildRun) publish(ctx context.Context) *attemptOutcome {
	if hook := r.c.d.BeforeRebuildPublish; hook != nil {
		hook(r.job.ID)
	}
	ref := "refs/heads/" + r.payload.IntegrationBranch
	var previous string
	err := r.git(ctx, func(ctx context.Context) error {
		cur, found, err := r.repo.LookupRef(ctx, ref)
		if found {
			previous = cur.ID
		}
		return err
	})
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: read %s: %w", r.ws.Slug, ref, err))
	}
	// The last look at the workspace, so that one archived, recloned or
	// deleted during the run keeps its integration branch.
	if out := r.readWorkspace(ctx); out != nil {
		return out
	}
	err = r.git(ctx, func(ctx context.Context) error {
		return r.repo.CompareAndSwapRef(ctx, ref, r.head, previous)
	})
	if errors.Is(err, gitrepo.ErrRefMoved) {
		return attemptFailed(attemptRetryable, reasonRefMoved, fmt.Errorf("rebuild of %s: %s was moved by another writer: %w", r.ws.Slug, ref, err))
	}
	if err != nil {
		return attemptFailed(attemptRetryable, reasonGitError, fmt.Errorf("rebuild of %s: move %s: %w", r.ws.Slug, ref, err))
	}
	head := r.head
	r.result.IntegrationHeadSHA = &head
	r.result.PreviousIntegrationHeadSHA = nil
	if previous != "" {
		r.result.PreviousIntegrationHeadSHA = &previous
	}
	return r.store(ctx)
}
