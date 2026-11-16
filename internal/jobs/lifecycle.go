package jobs

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ShutdownGrace is how long a stopping queue waits for the attempts it
// runs. An attempt still running then is cancelled and left unsettled: its
// job stays running in the store, and the next start's Recover queues it
// again with its retry count unchanged.
const ShutdownGrace = 30 * time.Second

// Recover returns every job the store holds as running to queued, keeping
// its retry count, error, payload and creation time: no attempt runs before
// the queue has started, so a running job is one whose attempt a stop
// interrupted, and it runs again from the start. Jobs in every other status
// are left as they are. The hub calls it in a start hook, before the
// dispatcher's first pass.
func (q *Queue) Recover(ctx context.Context) error {
	d, err := q.deps()
	if err != nil {
		return err
	}
	return d.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, updated_at = ? WHERE status = ?`,
			StatusQueued, d.Clock.Now().UTC().UnixNano(), StatusRunning)
		if err != nil {
			return fmt.Errorf("recover running jobs: %w", err)
		}
		return nil
	})
}

// stop ends the queue's work once Run's context is cancelled: no pass
// claims a job any more, and it waits up to ShutdownGrace for the attempts
// that run, whose outcomes are settled as they arrive. It returns as soon
// as the last of them is settled, at once when none runs. The attempts
// still running at the end of the grace are cancelled and abandoned: an
// outcome they return later is dropped, so that their jobs stay running
// for Recover.
func (q *Queue) stop(d Deps) {
	q.mu.Lock()
	q.stopping = true
	idle := len(q.running) == 0
	q.mu.Unlock()
	if idle {
		return
	}

	// No worker is reserved once stopping is set, so the wait sees every
	// attempt there is.
	ended := make(chan struct{})
	go func() {
		q.workers.Wait()
		close(ended)
	}()
	grace := d.Clock.NewTimer(ShutdownGrace)
	defer grace.Stop()
	select {
	case <-ended:
		return
	case <-grace.C():
	}

	q.mu.Lock()
	defer q.mu.Unlock()
	for _, w := range q.running {
		w.abandoned = true
		w.cancel()
	}
}
