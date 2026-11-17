package carrypatch

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
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

// RunRebuild runs one attempt of the running rebuild job j as the hub's
// rebuild handler runs it, and returns its outcome and the error the handler
// returns to the job queue.
func RunRebuild(ctx context.Context, c *Component, j jobs.Job) (AttemptOutcome, error) {
	out, err := rebuildHandler{c}.run(ctx, j)
	return AttemptOutcome{Kind: attemptKindNames[out.kind], Reason: out.reason, Text: out.text, Conflict: out.conflict}, err
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

// KeptOutcome is the outcome of the attempt of the job id the component
// keeps for the job's settlement, printed with every field, "" when it
// keeps none.
func KeptOutcome(c *Component, id string) string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out, ok := c.outcomes[id]
	if !ok {
		return ""
	}
	return fmt.Sprintf("%+v", out)
}

// MarkDeleted is markDeleted: it marks the stored patch p deleted at now in
// tx, as a removal does, without renumbering the others.
func MarkDeleted(ctx context.Context, tx *sql.Tx, p Patch, now time.Time) error {
	return markDeleted(ctx, tx, p, now)
}
