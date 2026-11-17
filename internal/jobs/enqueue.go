package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"uuid"
)

// DuplicateError is the refusal of an enqueue: the type and workspace have
// a pending job already, JobID. Read it with errors.As.
type DuplicateError struct {
	JobID string
}

func (e *DuplicateError) Error() string {
	return fmt.Sprintf("job %s of the workspace is pending already", e.JobID)
}

// errPayload is the error of an enqueue whose payload is not a JSON
// document.
var errPayload = errors.New("the job's payload is not a JSON document")

// Enqueue queues a job of typ for the workspace slug with payload, in tx:
// the job is stored when the caller commits, and not otherwise. When the
// type and workspace have a pending job (queued, running or failed), it
// stores nothing and returns a *DuplicateError naming that job. The write
// transactions of the store run one at a time, so the check and the insert
// cannot interleave with another enqueue's.
func (q *Queue) Enqueue(ctx context.Context, tx *sql.Tx, typ, slug string, payload json.RawMessage) (Job, error) {
	d, err := q.deps()
	if err != nil {
		return Job{}, err
	}
	pending, err := pendingJobOf(ctx, tx, typ, slug, "")
	if err != nil {
		return Job{}, err
	}
	if pending != "" {
		return Job{}, &DuplicateError{JobID: pending}
	}
	return q.insert(ctx, tx, d, typ, slug, payload)
}

// EnqueueFollowUp queues, in tx, the follow-up of the running job
// runningID: a job of typ for the workspace slug with payload, which runs
// once the running one has ended. The running job must be of typ and slug,
// and the only pending job of them. An unknown runningID, or one of another
// type or workspace, is ErrNotFound, so that a caller cannot queue a job
// for a workspace it did not name; otherwise a refusal is a
// *DuplicateError naming another pending job when there is one, else the
// named job.
func (q *Queue) EnqueueFollowUp(ctx context.Context, tx *sql.Tx, typ, slug, runningID string, payload json.RawMessage) (Job, error) {
	d, err := q.deps()
	if err != nil {
		return Job{}, err
	}
	running, err := readJob(ctx, tx, runningID)
	if err != nil {
		return Job{}, err
	}
	if running.Type != typ || running.Workspace != slug {
		return Job{}, ErrNotFound
	}
	other, err := pendingJobOf(ctx, tx, typ, slug, running.ID)
	if err != nil {
		return Job{}, err
	}
	if other != "" {
		return Job{}, &DuplicateError{JobID: other}
	}
	if running.Status != StatusRunning {
		return Job{}, &DuplicateError{JobID: running.ID}
	}
	return q.insert(ctx, tx, d, typ, slug, payload)
}

// insert stores a new queued job in tx and wakes the dispatcher for it.
func (q *Queue) insert(ctx context.Context, tx *sql.Tx, d Deps, typ, slug string, payload json.RawMessage) (Job, error) {
	if !json.Valid(payload) {
		return Job{}, errPayload
	}
	// In UTC and without a monotonic reading, as a read gives the time back,
	// so the job returned here equals the one Get returns after the commit.
	now := d.Clock.Now().UTC()
	j := Job{
		ID:        uuid.NewV4().String(),
		Type:      typ,
		Workspace: slug,
		Status:    StatusQueued,
		Payload:   append(json.RawMessage(nil), payload...),
		CreatedAt: now,
		UpdatedAt: now,
	}
	if err := insertJob(ctx, tx, j); err != nil {
		return Job{}, err
	}
	q.poke()
	return j, nil
}
