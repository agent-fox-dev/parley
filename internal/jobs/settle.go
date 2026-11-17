package jobs

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// Backoff bounds: the n-th retry of a job waits min(firstDelay × 2ⁿ,
// maxDelay) after the failure.
const (
	firstDelay = 2 * time.Second
	maxDelay   = 2 * time.Hour
)

// errResult is the error of a SetResult whose document is not JSON.
var errResult = errors.New("the job's result is not a JSON document")

// backoff is the wait before the next attempt of a job that has failed
// retryably n times: min(2 s × 2ⁿ, 2 h).
func backoff(n int) time.Duration {
	d := firstDelay
	for range n {
		d *= 2
		if d >= maxDelay {
			return maxDelay
		}
	}
	return d
}

// runAttempt runs one attempt of j with h. A panic of the handler is an
// attempt that failed retryably, so that a handler cannot end the process
// and a cause that passes is retried.
func runAttempt(ctx context.Context, h Handler, j Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("handler panicked: %v", p)
		}
	}()
	return h.Run(ctx, j)
}

// settle records the outcome runErr of an attempt of the job id in one
// transaction: completed on nil; failed with the next attempt after the
// backoff on a retryable error while retries are left; dead_letter on a
// permanent error and on a retryable one after the last retry. The hook of
// h is called in that transaction. A job that no longer exists or is no
// longer running (removed with its workspace, say) is left as it is, and
// the hook is not called. The dispatcher is woken once the attempt's worker
// is given up, after this.
func (q *Queue) settle(ctx context.Context, d Deps, h Handler, id string, runErr error) {
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		j, err := readJob(ctx, tx, id)
		if errors.Is(err, ErrNotFound) {
			return nil
		}
		if err != nil {
			return err
		}
		if j.Status != StatusRunning {
			return nil
		}
		outcome(d, &j, runErr)
		_, err = tx.ExecContext(ctx, `UPDATE jobs SET status = ?, retry_count = ?, next_attempt_at = ?, error = ?, detail = ?,
	completed_at = ?, updated_at = ? WHERE id = ?`, j.Status, j.RetryCount, timeArg(j.NextAttemptAt), j.Error,
			docArg(j.Detail), timeArg(j.CompletedAt), j.UpdatedAt.UnixNano(), j.ID)
		if err != nil {
			return fmt.Errorf("record the outcome: %w", err)
		}
		return callSettled(ctx, tx, d, h, j)
	})
	if err != nil {
		d.Logger.Error("job attempt not settled", "job_id", id, "error", err)
	}
}

// outcome sets on the running job j what an attempt that returned runErr
// leaves it as, at the clock's present.
func outcome(d Deps, j *Job, runErr error) {
	now := d.Clock.Now().UTC()
	j.UpdatedAt, j.NextAttemptAt, j.CompletedAt = now, nil, nil
	if runErr == nil {
		j.Status, j.Error, j.Detail, j.CompletedAt = StatusCompleted, nil, nil, &now
		return
	}
	text := runErr.Error()
	j.Error, j.Detail = &text, detailOf(runErr)
	if j.Detail != nil && !json.Valid(j.Detail) {
		d.Logger.Error("job failure detail is not JSON; not stored", "job_id", j.ID)
		j.Detail = nil
	}
	// The retry count rises only from running to failed, so that it stays
	// at MaxRetries when the failure after the last retry dead-letters the
	// job.
	if isPermanent(runErr) || j.RetryCount >= MaxRetries {
		j.Status, j.CompletedAt = StatusDeadLetter, &now
		return
	}
	j.Status, j.RetryCount = StatusFailed, j.RetryCount+1
	next := now.Add(backoff(j.RetryCount))
	j.NextAttemptAt = &next
}

// callSettled calls the settlement hook of h in tx with j as settled. It
// runs under a savepoint: a hook that fails or panics has its own writes
// undone and its failure logged, and the settlement commits anyway, since a
// job left running would hold its workspace until a restart. Only a
// failure of the savepoint itself is returned.
func callSettled(ctx context.Context, tx *sql.Tx, d Deps, h Handler, j Job) error {
	if _, err := tx.ExecContext(ctx, `SAVEPOINT job_settled`); err != nil {
		return fmt.Errorf("open the settlement hook's savepoint: %w", err)
	}
	if err := runHook(ctx, tx, h, j); err != nil {
		d.Logger.Error("job settlement hook failed; its writes are undone", "job_id", j.ID, "error", err)
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO job_settled`); err != nil {
			return fmt.Errorf("undo the settlement hook's writes: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE job_settled`); err != nil {
		return fmt.Errorf("release the settlement hook's savepoint: %w", err)
	}
	return nil
}

// runHook calls the settlement hook of h, turning a panic into its error.
// The job will be retried only when the settlement left it failed.
func runHook(ctx context.Context, tx *sql.Tx, h Handler, j Job) (err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("settlement hook panicked: %v", p)
		}
	}()
	return h.Settled(ctx, tx, j, j.Status == StatusFailed)
}

// SetResult replaces the result document of the running job id with doc
// (nil for null); readers see it once it returns. A job that is not
// running, or does not exist, is ErrNotRunning, and nothing changes.
func (q *Queue) SetResult(ctx context.Context, id string, doc json.RawMessage) error {
	d, err := q.deps()
	if err != nil {
		return err
	}
	if doc != nil && !json.Valid(doc) {
		return errResult
	}
	return d.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE jobs SET result = ?, updated_at = ? WHERE id = ? AND status = ?`,
			docArg(doc), d.Clock.Now().UTC().UnixNano(), id, StatusRunning)
		if err != nil {
			return fmt.Errorf("store the result of job %s: %w", id, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return fmt.Errorf("store the result of job %s: %w", id, err)
		}
		if n == 0 {
			return ErrNotRunning
		}
		return nil
	})
}

// docArg is a document as the argument of a nullable JSON column.
func docArg(doc json.RawMessage) any {
	if doc == nil {
		return nil
	}
	return string(doc)
}

// timeArg is a time as the argument of a nullable UnixNano column.
func timeArg(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.UnixNano()
}
