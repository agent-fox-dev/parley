package jobs

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

// MaxRunning is the most attempts the queue runs at once.
const MaxRunning = 4

// MaxRetries is the most retries of a job: the retryable failure of an
// attempt at this retry count dead-letters the job.
const MaxRetries = 20

// DefaultInterval is how often the dispatcher passes when Deps sets no
// interval, so that a queued job starts within seconds even without a wake.
const DefaultInterval = time.Second

// Clock is the queue's time: the time a job is stamped with, and the timers
// the dispatcher runs on, so that a test controls both.
type Clock interface {
	// Now is the current time.
	Now() time.Time
	// NewTimer returns a timer that fires once, d from now.
	NewTimer(d time.Duration) Timer
}

// Timer is a timer of a Clock.
type Timer interface {
	// C is the channel the timer sends the time on when it fires.
	C() <-chan time.Time
	// Stop prevents the timer from firing; it reports whether the timer was
	// still pending.
	Stop() bool
}

// SystemClock is the Clock of a running hub: now for the time (time.Now when
// nil) and the runtime's timers.
func SystemClock(now func() time.Time) Clock {
	if now == nil {
		now = time.Now
	}
	return systemClock{now: now}
}

type systemClock struct{ now func() time.Time }

func (c systemClock) Now() time.Time { return c.now() }

func (c systemClock) NewTimer(d time.Duration) Timer { return systemTimer{time.NewTimer(d)} }

type systemTimer struct{ t *time.Timer }

func (t systemTimer) C() <-chan time.Time { return t.t.C }

func (t systemTimer) Stop() bool { return t.t.Stop() }

// Deps are what a queue runs on.
type Deps struct {
	// Store holds the jobs.
	Store *store.Store
	// Clock is the queue's time; SystemClock(time.Now) when nil.
	Clock Clock
	// Logger receives what the queue cannot return; nothing is logged when
	// nil.
	Logger *slog.Logger
	// Interval is the time between the dispatcher's passes; DefaultInterval
	// when 0.
	Interval time.Duration
	// NoWake leaves the dispatcher to its interval alone, so that a test
	// decides when a pass runs.
	NoWake bool
}

// Queue is the job queue. Its zero value takes registrations; Bind gives
// it what it runs on. Its methods are safe for concurrent use.
type Queue struct {
	mu sync.Mutex
	d  Deps
	// wake holds one token: an enqueue leaves it for the dispatcher, which
	// passes at once instead of at its next interval.
	wake chan struct{}
	// handlers are the registered handlers, by job type.
	handlers map[string]Handler
	// limits are the most attempts of a type that run at once, by job type
	// (Limit); a type without one is bounded by MaxRunning alone.
	limits map[string]int
	// running are the attempts this queue has claimed and not yet seen
	// end, by job id. Its size is what the limit of MaxRunning counts.
	running map[string]*worker
	// stopping is set once the queue stops claiming: a pass then claims
	// nothing.
	stopping bool
	// removals are the cancellations of the attempts of jobs a workspace
	// removal deleted that have not ended yet, each closed once it has: a
	// stop waits for them, since each needs the store.
	removals map[chan struct{}]bool
}

// worker is an attempt of a job the queue runs.
type worker struct {
	// typ is the type of the attempt's job.
	typ string
	// cancel cancels the context of the attempt.
	cancel context.CancelFunc
	// done is closed once the attempt has ended and its outcome is
	// settled.
	done chan struct{}
	// abandoned is set, under the queue's lock, when a stop has given up
	// on the attempt: its outcome is not settled.
	abandoned bool
	// settling is set, under the queue's lock, once the attempt has
	// returned and its outcome is being settled: a stop no longer abandons
	// it, and waits for the settlement to end.
	settling bool
}

// ErrNotFound is the error of an operation on a job that does not exist.
var ErrNotFound = errors.New("job not found")

// errNotBound is the error of an operation that needs the store before
// Bind.
var errNotBound = errors.New("the job queue is not bound")

// New returns a queue bound to d.
func New(d Deps) *Queue {
	q := &Queue{}
	q.Bind(d)
	return q
}

// Bind gives the queue what it runs on, filling in the defaults of Deps.
func (q *Queue) Bind(d Deps) {
	if d.Clock == nil {
		d.Clock = SystemClock(time.Now)
	}
	if d.Logger == nil {
		d.Logger = slog.New(slog.DiscardHandler)
	}
	if d.Interval <= 0 {
		d.Interval = DefaultInterval
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	q.d = d
	if q.wake == nil {
		q.wake = make(chan struct{}, 1)
	}
}

// deps returns what the queue runs on, or errNotBound before Bind.
func (q *Queue) deps() (Deps, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.d.Store == nil {
		return Deps{}, errNotBound
	}
	return q.d, nil
}

// poke wakes the dispatcher for a pass, unless the queue runs without
// wakes. A pass claims inside a write transaction of the store, so one
// woken while the poking transaction is still open waits for it to end and
// then sees what it wrote.
func (q *Queue) poke() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.d.NoWake || q.wake == nil {
		return
	}
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// Get returns the job id as committed; a job that does not exist is
// ErrNotFound.
func (q *Queue) Get(ctx context.Context, id string) (Job, error) {
	d, err := q.deps()
	if err != nil {
		return Job{}, err
	}
	return readJob(ctx, d.Store.Main(), id)
}

// List returns up to limit jobs of typ for the workspace slug, in status
// when it is not "", newest first: created_at as a record shows it, in
// whole seconds, descending, then id descending. after, when not nil, is
// the Position of the last job of the previous page.
func (q *Queue) List(ctx context.Context, typ, slug, status string, after *ListPosition, limit int) ([]Job, error) {
	d, err := q.deps()
	if err != nil {
		return nil, err
	}
	return listJobs(ctx, d.Store.Main(), typ, slug, status, after, limit)
}

// claim is a job a pass claimed, with the handler of its type and the
// worker that runs its attempt.
type claim struct {
	job     Job
	handler Handler
	ctx     context.Context
	worker  *worker
}

// Pass is one pass of the dispatcher. It returns every failed job whose
// next attempt is due to queued, then claims queued jobs, oldest first, for
// the types that have a handler: at most MaxRunning attempts run at once,
// and at most one per type and workspace. Each claimed job's attempt starts
// on a goroutine of its own once the claim has committed. A pass that finds
// nothing to do opens no write transaction, and a pass of a stopping queue
// does nothing.
func (q *Queue) Pass(ctx context.Context) error {
	return q.pass(ctx, false)
}

// pass is Pass. A woken pass skips the look for work outside a transaction:
// the wake may come from a transaction that is still open, whose job only
// the write transaction, which waits for it to end, can see.
func (q *Queue) pass(ctx context.Context, woken bool) error {
	d, err := q.deps()
	if err != nil {
		return err
	}
	q.mu.Lock()
	stopping, room := q.stopping, len(q.running) < MaxRunning
	types := slices.Sorted(maps.Keys(q.handlers))
	// A type at its limit has nothing a pass could claim, like a queue
	// without room.
	atLimit := map[string]int{}
	for _, w := range q.running {
		atLimit[w.typ]++
	}
	types = slices.DeleteFunc(types, func(typ string) bool {
		limit := q.limits[typ]
		return limit > 0 && atLimit[typ] >= limit
	})
	q.mu.Unlock()
	if stopping {
		return nil
	}
	if !room {
		types = nil
	}
	if !woken {
		work, err := hasWork(ctx, d.Store.Main(), d.Clock.Now(), types)
		if err != nil || !work {
			return err
		}
	}

	// An attempt outlives the pass that started it: the queue cancels it,
	// not the pass's caller.
	base := context.WithoutCancel(ctx)
	var claimed []claim
	err = d.Store.Tx(ctx, func(tx *sql.Tx) error {
		now := d.Clock.Now().UTC()
		if err := requeueDue(ctx, tx, now); err != nil {
			return err
		}
		if len(types) == 0 {
			return nil
		}
		candidates, err := queuedJobs(ctx, tx, types)
		if err != nil {
			return err
		}
		// The type and workspace of a job this pass claimed: one of each runs
		// at a time, also when two of its jobs were queued.
		taken := map[[2]string]bool{}
		for _, j := range candidates {
			key := [2]string{j.Type, j.Workspace}
			if taken[key] {
				continue
			}
			c, full := q.reserve(base, j)
			if full {
				break
			}
			if c.worker == nil {
				continue
			}
			ok, err := claimJob(ctx, tx, j.ID, now)
			if err != nil || !ok {
				q.drop(j.ID)
				if err != nil {
					return err
				}
				continue
			}
			taken[key] = true
			c.job.Status, c.job.Result, c.job.UpdatedAt = StatusRunning, nil, now
			claimed = append(claimed, c)
		}
		return nil
	})
	if err != nil {
		for _, c := range claimed {
			q.drop(c.job.ID)
		}
		return err
	}
	for _, c := range claimed {
		go q.work(d, c)
	}
	return nil
}

// reserve takes a worker for j when the queue has a handler for its type
// and fewer than MaxRunning attempts run; full reports that it has not and
// will not before an attempt ends. Reserving under the queue's lock keeps
// the limit across passes that run at the same time.
func (q *Queue) reserve(base context.Context, j Job) (c claim, full bool) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.stopping || len(q.running) >= MaxRunning {
		return claim{}, true
	}
	h, ok := q.handlers[j.Type]
	if _, runs := q.running[j.ID]; !ok || runs {
		return claim{}, false
	}
	if limit := q.limits[j.Type]; limit > 0 {
		n := 0
		for _, w := range q.running {
			if w.typ == j.Type {
				n++
			}
		}
		if n >= limit {
			return claim{}, false
		}
	}
	ctx, cancel := context.WithCancel(base)
	w := &worker{typ: j.Type, cancel: cancel, done: make(chan struct{})}
	if q.running == nil {
		q.running = map[string]*worker{}
	}
	q.running[j.ID] = w
	return claim{job: j, handler: h, ctx: ctx, worker: w}, false
}

// drop gives up the worker of the job id: its claim did not commit, or its
// attempt has ended. The dispatcher is woken, since a worker is free.
func (q *Queue) drop(id string) {
	q.mu.Lock()
	w, ok := q.running[id]
	if ok {
		delete(q.running, id)
	}
	q.mu.Unlock()
	if !ok {
		return
	}
	w.cancel()
	close(w.done)
	q.poke()
}

// work runs the attempt of c and settles its outcome, unless a stop has
// abandoned the attempt.
func (q *Queue) work(d Deps, c claim) {
	defer q.drop(c.job.ID)
	err := runAttempt(c.ctx, c.handler, c.job)
	q.mu.Lock()
	abandoned := c.worker.abandoned
	c.worker.settling = !abandoned
	q.mu.Unlock()
	if abandoned {
		return
	}
	q.settle(context.WithoutCancel(c.ctx), d, c.handler, c.job.ID, err)
}

// Run is the dispatcher: it passes at once, then every Interval and
// whenever it is woken, until ctx is cancelled. Then it stops: it claims
// nothing more and waits up to ShutdownGrace for the attempts that run
// (stop).
func (q *Queue) Run(ctx context.Context) {
	d, err := q.deps()
	if err != nil {
		return
	}
	q.mu.Lock()
	wake := q.wake
	q.mu.Unlock()
	woken := false
	for {
		if err := q.pass(ctx, woken); err != nil && ctx.Err() == nil {
			d.Logger.Error("job dispatcher pass", "error", err)
		}
		woken = false
		t := d.Clock.NewTimer(d.Interval)
		select {
		case <-ctx.Done():
			t.Stop()
			q.stop(d)
			return
		case <-t.C():
		case <-wake:
			t.Stop()
			woken = true
		}
	}
}

// inTypes is the SQL condition and arguments of a job of one of types.
func inTypes(types []string) (string, []any) {
	args := make([]any, len(types))
	for i, t := range types {
		args[i] = t
	}
	return `type IN (?` + strings.Repeat(`, ?`, len(types)-1) + `)`, args
}

// hasWork reports whether a pass has something to do at now: a failed job
// whose next attempt is due, or a queued job of one of types that a pass
// could claim, one whose type and workspace have no running job (queuedJobs),
// so that a follow-up waiting for its running job costs no write
// transaction.
func hasWork(ctx context.Context, q querier, now time.Time, types []string) (bool, error) {
	query := `SELECT EXISTS (SELECT 1 FROM jobs WHERE status = ? AND next_attempt_at <= ?)`
	args := []any{StatusFailed, now.UnixNano()}
	if len(types) > 0 {
		cond, targs := inTypes(types)
		query += ` OR EXISTS (SELECT 1 FROM jobs WHERE status = ? AND ` + cond + ` AND ` + noneRunning + `)`
		args = append(append(append(args, StatusQueued), targs...), StatusRunning)
	}
	var work bool
	if err := q.QueryRowContext(ctx, query, args...).Scan(&work); err != nil {
		return false, fmt.Errorf("look for due jobs: %w", err)
	}
	return work, nil
}

// requeueDue returns, in tx, every failed job whose next attempt is due at
// now to queued, keeping its retry count and error.
func requeueDue(ctx context.Context, tx *sql.Tx, now time.Time) error {
	_, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, next_attempt_at = NULL, updated_at = ?
WHERE status = ? AND next_attempt_at <= ?`, StatusQueued, now.UnixNano(), StatusFailed, now.UnixNano())
	if err != nil {
		return fmt.Errorf("requeue due jobs: %w", err)
	}
	return nil
}

// noneRunning is the SQL condition, with the running status as its
// argument, that a job's type and workspace have no running job.
const noneRunning = `NOT EXISTS (SELECT 1 FROM jobs r WHERE r.type = jobs.type AND r.workspace_slug = jobs.workspace_slug AND r.status = ?)`

// queuedJobs reads, in tx, the queued jobs of types whose type and
// workspace have no running job, in claim order: created_at as a record
// shows it, ascending, then id ascending.
func queuedJobs(ctx context.Context, tx *sql.Tx, types []string) ([]Job, error) {
	cond, args := inTypes(types)
	rows, err := tx.QueryContext(ctx, `SELECT `+jobColumns+` FROM jobs WHERE status = ? AND `+cond+` AND `+noneRunning+`
	ORDER BY `+secondsOf+`, id`, append(append([]any{StatusQueued}, args...), StatusRunning)...)
	if err != nil {
		return nil, fmt.Errorf("read queued jobs: %w", err)
	}
	defer rows.Close()
	var jobs []Job
	for rows.Next() {
		j, err := scanJob(rows)
		if err != nil {
			return nil, fmt.Errorf("read queued jobs: %w", err)
		}
		jobs = append(jobs, j)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read queued jobs: %w", err)
	}
	return jobs, nil
}

// claimJob sets the queued job id running in tx, emptying its result; it
// reports false when the job was no longer queued, a claim lost. Cancel
// takes a job on the same condition, so a job is claimed or cancelled,
// never both.
func claimJob(ctx context.Context, tx *sql.Tx, id string, now time.Time) (bool, error) {
	res, err := tx.ExecContext(ctx, `UPDATE jobs SET status = ?, result = NULL, updated_at = ? WHERE id = ? AND status = ?`,
		StatusRunning, now.UnixNano(), id, StatusQueued)
	if err != nil {
		return false, fmt.Errorf("claim job %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim job %s: %w", id, err)
	}
	return n == 1, nil
}

// Depth is the number of jobs in each status, for every type with a
// registered handler: all six statuses are there, 0 for one no job is in.
func (q *Queue) Depth(ctx context.Context) (map[string]map[string]int, error) {
	d, err := q.deps()
	if err != nil {
		return nil, err
	}
	q.mu.Lock()
	depth := make(map[string]map[string]int, len(q.handlers))
	for typ := range q.handlers {
		depth[typ] = make(map[string]int, len(Statuses))
		for _, status := range Statuses {
			depth[typ][status] = 0
		}
	}
	q.mu.Unlock()
	if len(depth) == 0 {
		return depth, nil
	}
	err = countJobs(ctx, d.Store.Main(), func(typ, status string, n int) {
		if byStatus, ok := depth[typ]; ok {
			byStatus[status] = n
		}
	})
	return depth, err
}

// RemoveWorkspace deletes every job of the workspace slug, of every type
// and status, in tx, the transaction of the workspace's removal; it is a
// remover of the workspace's owned data (ADR 07). An attempt of a deleted
// job is cancelled once that transaction has committed, and its outcome
// then finds no job to settle, so it is dropped. A rolled-back removal
// cancels nothing.
func (q *Queue) RemoveWorkspace(ctx context.Context, tx *sql.Tx, slug string) error {
	ids, err := deleteWorkspaceJobs(ctx, tx, slug)
	if err != nil {
		return err
	}
	// A claim commits in a write transaction too, so while tx is open the
	// attempts the queue runs are all it will run of these jobs.
	q.mu.Lock()
	var attempts []string
	for _, id := range ids {
		if _, ok := q.running[id]; ok {
			attempts = append(attempts, id)
		}
	}
	d := q.d
	var done chan struct{}
	if len(attempts) > 0 {
		done = make(chan struct{})
		if q.removals == nil {
			q.removals = map[chan struct{}]bool{}
		}
		q.removals[done] = true
	}
	q.mu.Unlock()
	if done == nil {
		return nil
	}
	go func() {
		defer q.removalEnded(done)
		q.cancelRemoved(context.WithoutCancel(ctx), d, attempts)
	}()
	return nil
}

// removalEnded marks the cancellation done as ended.
func (q *Queue) removalEnded(done chan struct{}) {
	q.mu.Lock()
	delete(q.removals, done)
	q.mu.Unlock()
	close(done)
}

// cancelRemoved cancels the attempts of the jobs ids that no longer exist.
// A remover sees only its transaction and the store has no commit hook, so
// this waits for a write transaction of its own: the store runs them one
// at a time, in the order they are asked for, so the removal's has ended
// by then, and the jobs are gone only if it committed.
func (q *Queue) cancelRemoved(ctx context.Context, d Deps, ids []string) {
	var gone []string
	err := d.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, id := range ids {
			_, err := readJob(ctx, tx, id)
			if errors.Is(err, ErrNotFound) {
				gone = append(gone, id)
				continue
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		d.Logger.Error("attempts of removed jobs not cancelled", "job_ids", ids, "error", err)
		return
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, id := range gone {
		if w, ok := q.running[id]; ok {
			w.cancel()
		}
	}
}
