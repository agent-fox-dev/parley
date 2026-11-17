package carrypatch

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// reasonRebuildClean is the reason of the parley.patch.update of a patch in
// conflict that a rebuild applied again and the publish returned to active.
const reasonRebuildClean = "rebuild_clean"

// publishStopped carries the outcome of a publish that ends the attempt
// out of the publish's store transaction, which it rolls back.
type publishStopped struct{ out *attemptOutcome }

func (s publishStopped) Error() string { return s.out.text }

// publish moves the integration branch named on the job to the head under
// construction in one compare-and-swap against the head it reads just
// before, creating the branch when it does not exist, and stores both
// heads on the job; with no patch applied, the head is the upstream base.
// It is the one ref the run moves.
//
// The changes of the patch list that the new head proves are prepared in
// one store transaction, the swap is made inside it, and the transaction
// commits only when the swap won: a ref and the store cannot commit as
// one, and this order makes a lost swap leave no trace in the patch list.
// Another writer that moves the branch in between makes the swap fail, and
// the attempt with it, and a workspace that is no longer ready just before
// the swap ends the attempt as at its start. The heads are stored on the
// job once the transaction committed, since the job queue writes a job's
// result in a store transaction of its own.
func (r *rebuildRun) publish(ctx context.Context) *attemptOutcome {
	if hook := r.c.d.BeforeRebuildPublish; hook != nil {
		hook(r.job.ID)
	}
	if r.c.d.Events == nil {
		return attemptFailed(attemptRetryable, reasonGitError, errNotBound)
	}
	ref := "refs/heads/" + r.payload.IntegrationBranch
	// The branch is read before the store transaction, which holds the
	// store's one writer slot: the swap against what was read catches any
	// move in between.
	var previous string
	err := r.git(ctx, func(ctx context.Context) error {
		cur, found, err := r.repo.LookupRef(ctx, ref)
		if found {
			previous = cur.ID
		}
		return err
	})
	if err != nil {
		return r.failed(ctx, fmt.Errorf("rebuild of %s: publish: read %s: %w", r.ws.Slug, ref, err))
	}
	var removed int
	err = r.c.d.Store.Tx(ctx, func(tx *sql.Tx) error {
		var err error
		if removed, err = r.settlePatches(ctx, tx); err != nil {
			return err
		}
		if hook := r.c.d.BeforeRebuildSwap; hook != nil {
			hook(r.job.ID)
		}
		// The last look at the workspace, so that one archived, recloned or
		// deleted during the run keeps its integration branch. In the
		// transaction, no such change can come between it and the commit.
		ws, found, err := workspace.BySlug(ctx, tx, r.job.Workspace)
		if err != nil {
			return fmt.Errorf("read the workspace %s: %w", r.job.Workspace, err)
		}
		if out := r.admit(ws, found); out != nil {
			return publishStopped{out}
		}
		err = r.git(ctx, func(ctx context.Context) error {
			return r.repo.CompareAndSwapRef(ctx, ref, r.head, previous)
		})
		if err != nil {
			return fmt.Errorf("move %s: %w", ref, err)
		}
		return nil
	})
	if stopped, ok := errors.AsType[publishStopped](err); ok {
		return stopped.out
	}
	if errors.Is(err, gitrepo.ErrRefMoved) {
		return attemptFailed(attemptRetryable, reasonRefMoved, fmt.Errorf("rebuild of %s: %s was moved by another writer: %w", r.ws.Slug, ref, err))
	}
	if err != nil {
		return r.failed(ctx, fmt.Errorf("rebuild of %s: publish: %w", r.ws.Slug, err))
	}
	r.removed = removed
	head := r.head
	r.result.IntegrationHeadSHA = &head
	r.result.PreviousIntegrationHeadSHA = nil
	if previous != "" {
		r.result.PreviousIntegrationHeadSHA = &previous
	}
	return r.store(ctx)
}

// settlePatches makes, in tx, the changes of the patch list that the
// published head proves, and returns how many patches it removed. It
// follows the patch list as it is now, not as the run listed it: a patch
// the run skipped as merged upstream is soft-deleted only while it is
// still merged_upstream, and one the run applied is returned to active,
// with no conflict files, only while it is in conflict; a patch a caller
// changed during the run keeps what the caller gave it. The remaining
// patches are renumbered 1..N in their present order, which emits no event
// of its own. Each removed and each returned patch gets its event.
func (r *rebuildRun) settlePatches(ctx context.Context, tx *sql.Tx) (int, error) {
	done := make(map[string]patchResult, len(r.results))
	for _, res := range r.results {
		done[res.PatchID] = res
	}
	slug := r.ws.Slug
	live, err := ListLivePatches(ctx, tx, slug)
	if err != nil {
		return 0, err
	}
	now := r.c.now().UTC().Truncate(time.Second)
	removed := 0
	kept := make([]Patch, 0, len(live))
	for _, p := range live {
		res, ran := done[p.ID]
		switch {
		case ran && p.Status == StatusMergedUpstream && res.Status == resultSkipped &&
			res.SkippedReason != nil && *res.SkippedReason == skippedMergedUpstream:
			if err := markDeleted(ctx, tx, p, now); err != nil {
				return 0, err
			}
			if _, err := r.c.d.Events.Emit(ctx, tx, event.Input{
				Type: EventPatchDelete, Workspace: &slug, ActorType: event.ActorSystem,
				ResourceType: "patch", ResourceID: p.ID,
				Metadata: map[string]any{"branch_name": p.BranchName, "reason": skippedMergedUpstream},
			}); err != nil {
				return 0, err
			}
			removed++
			continue
		case ran && p.Status == StatusConflict && res.Status == resultSuccess:
			next := p
			next.Status, next.ConflictFiles, next.UpdatedAt = StatusActive, []string{}, now
			if err := writeFields(ctx, tx, next); err != nil {
				return 0, err
			}
			if _, err := r.c.d.Events.Emit(ctx, tx, event.Input{
				Type: EventPatchUpdate, Workspace: &slug, ActorType: event.ActorSystem,
				ResourceType: "patch", ResourceID: p.ID,
				Metadata: map[string]any{"branch_name": p.BranchName, "changed": []string{"status"},
					"from": StatusConflict, "to": StatusActive, "reason": reasonRebuildClean},
			}); err != nil {
				return 0, err
			}
			p = next
		}
		kept = append(kept, p)
	}
	if _, err := renumber(ctx, tx, kept, now); err != nil {
		return 0, err
	}
	return removed, nil
}
