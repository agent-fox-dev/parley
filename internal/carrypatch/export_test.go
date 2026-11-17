package carrypatch

import (
	"context"

	"github.com/agent-fox-dev/parley/internal/jobs"
)

// DeleteBackupRef is deleteBackupRef, which an add runs once its
// transaction committed, whatever became of its request.
func DeleteBackupRef(ctx context.Context, c *Component, slug, branch string) {
	c.deleteBackupRef(ctx, slug, branch)
}

// AttemptOutcome is the outcome of one attempt of a rebuild job as a test
// reads it: its kind (completed, retryable, permanent or ended), its reason
// word and its text.
type AttemptOutcome struct {
	Kind, Reason, Text string
}

// attemptKindNames are the names of the kinds of an attempt's outcome.
var attemptKindNames = map[attemptKind]string{
	attemptCompleted: "completed",
	attemptRetryable: "retryable",
	attemptPermanent: "permanent",
	attemptEnded:     "ended",
}

// RunRebuildAttempt runs one attempt of the running rebuild job j with the
// component c and returns its outcome.
func RunRebuildAttempt(ctx context.Context, c *Component, j jobs.Job) AttemptOutcome {
	out := c.runRebuildAttempt(ctx, j)
	return AttemptOutcome{Kind: attemptKindNames[out.kind], Reason: out.reason, Text: out.text}
}

// PatchResult is what a rebuild did with one patch, as the job's
// patch_results lists it.
type PatchResult = patchResult

// The statuses of a patch's result and the reasons a run skips a patch.
const (
	ResultSuccess         = resultSuccess
	ResultConflict        = resultConflict
	ResultSkipped         = resultSkipped
	SkippedDisabled       = skippedDisabled
	SkippedMergedUpstream = skippedMergedUpstream
	SkippedBranchNotFound = skippedBranchNotFound
	SkippedNothingToApply = skippedNothingToApply
)
