package carrypatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
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
	// Conflict is the conflict report a fail_fast attempt stopped with.
	Conflict json.RawMessage
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
	return AttemptOutcome{Kind: attemptKindNames[out.kind], Reason: out.reason, Text: out.text, Conflict: out.conflict}
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

// RecordRerereEntries records, in the transaction tx, an entry of the
// workspace slug for each of the conflicts that has none, at the time at.
func RecordRerereEntries(ctx context.Context, tx *sql.Tx, slug string, conflicts []gitrepo.RerereConflict, at time.Time) error {
	return recordRerereEntries(ctx, tx, slug, conflicts, at, false)
}
