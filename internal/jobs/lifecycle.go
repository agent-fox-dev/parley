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
// that run, whose outcomes are settled as they arrive, and for the
// cancellations of removed jobs' attempts, which use the store the hub
// closes once Run has returned. It returns as soon as the last of them has
// ended, at once when none is under way. At the end of the grace, the
// attempts still running are cancelled and abandoned: an outcome they
// return later is dropped, so that their jobs stay running for Recover.
// The settlements already under way then are waited for, since they write
// to the store; a cancellation of removed jobs' attempts still under way is
// not, so that a stop is never held beyond the grace by the store.
func (q *Queue) stop(d Deps) {
	q.mu.Lock()
	q.stopping = true
	q.mu.Unlock()

	// No worker is reserved once stopping is set; a removal may still start
	// a cancellation, so the work under way is looked for again each time a
	// part of it ends. Nothing here starts a goroutine, so that nothing is
	// left waiting for an attempt that ignores its cancellation.
	next := q.underWay()
	if next == nil {
		return
	}
	grace := d.Clock.NewTimer(ShutdownGrace)
	defer grace.Stop()
	for next != nil {
		select {
		case <-next:
			next = q.underWay()
		case <-grace.C():
			q.abandon()
			return
		}
	}
}

// abandon cancels and abandons every attempt still running at the end of a
// stop's grace, and waits for the settlements already under way.
func (q *Queue) abandon() {
	q.mu.Lock()
	var settling []chan struct{}
	for _, w := range q.running {
		if w.settling {
			settling = append(settling, w.done)
			continue
		}
		w.abandoned = true
		w.cancel()
	}
	q.mu.Unlock()
	for _, done := range settling {
		<-done
	}
}

// underWay is the channel of an attempt or of a removal's cancellation
// that has not ended yet, closed once it has; nil when there is none.
func (q *Queue) underWay() <-chan struct{} {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, w := range q.running {
		return w.done
	}
	for done := range q.removals {
		return done
	}
	return nil
}
