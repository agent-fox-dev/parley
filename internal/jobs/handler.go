package jobs

import (
	"context"
	"database/sql"
)

// Handler runs the jobs of one type. The queue calls Run for every attempt
// of a job it claimed, on a goroutine of its own, and Settled inside the
// transaction that records the attempt's outcome.
type Handler interface {
	// Run makes one attempt of j, which the queue has set running. A nil
	// error completes the job. ctx is cancelled when the queue gives up on
	// the attempt; an attempt runs from the start, never from where an
	// earlier one stopped.
	Run(ctx context.Context, j Job) error
	// Settled is called in tx, the transaction that records the outcome of
	// an attempt of j, with j as recorded and whether the job will be
	// retried. It must not open a transaction of the store itself.
	Settled(ctx context.Context, tx *sql.Tx, j Job, willRetry bool) error
}

// Register makes h the handler of the jobs of typ, replacing the one it
// had. It may be called before Bind: the hub registers handlers while it is
// composed. A job of a type without a handler stays queued until one is
// registered.
func (q *Queue) Register(typ string, h Handler) {
	q.mu.Lock()
	if q.handlers == nil {
		q.handlers = map[string]Handler{}
	}
	q.handlers[typ] = h
	q.mu.Unlock()
	q.poke()
}

// Limit makes the queue run at most n attempts of the type typ at once,
// whatever else it runs: a job of the type that finds n running stays
// queued, with no attempt counted, until one of them ends. An n of zero or
// less takes the limit away. It is for a type whose attempts are costly
// enough to be bounded apart from the queue's own MaxRunning.
func (q *Queue) Limit(typ string, n int) {
	q.mu.Lock()
	if q.limits == nil {
		q.limits = map[string]int{}
	}
	if n > 0 {
		q.limits[typ] = n
	} else {
		delete(q.limits, typ)
	}
	q.mu.Unlock()
	q.poke()
}
