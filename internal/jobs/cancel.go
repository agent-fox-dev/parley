package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// ErrNotCancellable is the error of a cancel of a job that is not queued.
var ErrNotCancellable = errors.New("job not cancellable: only a queued job is")

// ErrNotRequeueable is the error of a requeue of a job that is not
// dead_letter.
var ErrNotRequeueable = errors.New("job not requeueable: only a dead-lettered job is")

// Cancel cancels the queued job id in tx, and returns it as cancelled. A
// job in any other status is ErrNotCancellable, one that does not exist
// ErrNotFound. The claim of a pass takes a job on the same condition, that
// it is queued, in a write transaction too, and the store runs those one
// at a time: of a cancel and a claim of the same job, exactly one takes
// effect.
func (q *Queue) Cancel(ctx context.Context, tx *sql.Tx, id string) (Job, error) {
	d, err := q.deps()
	if err != nil {
		return Job{}, err
	}
	now := d.Clock.Now().UTC().UnixNano()
	j, err := scanJob(tx.QueryRowContext(ctx, `UPDATE jobs SET status = ?, completed_at = ?, updated_at = ?
WHERE id = ? AND status = ? RETURNING `+jobColumns, StatusCancelled, now, now, id, StatusQueued))
	if errors.Is(err, sql.ErrNoRows) {
		if _, err := readJob(ctx, tx, id); err != nil {
			return Job{}, err
		}
		return Job{}, ErrNotCancellable
	}
	if err != nil {
		return Job{}, fmt.Errorf("cancel job %s: %w", id, err)
	}
	return j, nil
}

// Requeue queues the dead-lettered job id again in tx, as a fresh job
// that keeps its id, created_at and payload: retry count 0, no error, no
// detail, no next attempt and no completion. A job in any other status is
// ErrNotRequeueable, whatever the other jobs of its type and workspace
// are; a dead-lettered one while its type and workspace have a pending job
// is a *DuplicateError naming that job; one that does not exist is
// ErrNotFound. The dispatcher is woken for the job.
func (q *Queue) Requeue(ctx context.Context, tx *sql.Tx, id string) (Job, error) {
	d, err := q.deps()
	if err != nil {
		return Job{}, err
	}
	j, err := readJob(ctx, tx, id)
	if err != nil {
		return Job{}, err
	}
	if j.Status != StatusDeadLetter {
		return Job{}, ErrNotRequeueable
	}
	pending, err := pendingJobOf(ctx, tx, j.Type, j.Workspace, j.ID)
	if err != nil {
		return Job{}, err
	}
	if pending != "" {
		return Job{}, &DuplicateError{JobID: pending}
	}
	j, err = scanJob(tx.QueryRowContext(ctx, `UPDATE jobs SET status = ?, retry_count = 0, error = NULL, detail = NULL,
	next_attempt_at = NULL, completed_at = NULL, updated_at = ? WHERE id = ? RETURNING `+jobColumns,
		StatusQueued, d.Clock.Now().UTC().UnixNano(), id))
	if err != nil {
		return Job{}, fmt.Errorf("requeue job %s: %w", id, err)
	}
	q.poke()
	return j, nil
}

// Hasten makes the failed job of typ and the workspace slug due at once, in
// tx: it is queued again with its retry count and error, as a pass does
// when its next attempt is due, and the dispatcher is woken for it. It
// reports whether there was such a job. It is for a job whose reason to
// wait has gone: what it failed on has changed.
func (q *Queue) Hasten(ctx context.Context, tx *sql.Tx, typ, slug string) (bool, error) {
	d, err := q.deps()
	if err != nil {
		return false, err
	}
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, next_attempt_at = NULL, updated_at = ?
WHERE type = ? AND workspace_slug = ? AND status = ?`, StatusQueued, d.Clock.Now().UTC().UnixNano(), typ, slug, StatusFailed)
	if err != nil {
		return false, fmt.Errorf("hasten the %s job of %s: %w", typ, slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil || n == 0 {
		return false, err
	}
	q.poke()
	return true, nil
}
