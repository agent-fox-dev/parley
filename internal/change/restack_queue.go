package change

import (
	"context"
	"database/sql"
	"fmt"
	"sync"
	"time"

	"github.com/agent-fox-dev/parley/internal/gitrepo"
	"github.com/agent-fox-dev/parley/internal/store"
)

// RestackStep adds the pending restacks: at most one per change, the
// restack the hub owes it, kept until the restack ends so that one pending
// when the hub stops runs after the next start. seq orders them as they
// were recorded; a record replaced by a newer one gets a higher seq. onto
// is the commit the change must sit on when its record names one, such as
// the commit the change below landed as, ” otherwise. It applies after
// SchemaStep.
var RestackStep = store.Step{
	Name:   "change_restacks",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE change_restacks (
	seq INTEGER PRIMARY KEY AUTOINCREMENT,
	workspace_slug TEXT NOT NULL,
	number INTEGER NOT NULL,
	trigger TEXT NOT NULL,
	onto TEXT NOT NULL DEFAULT '',
	UNIQUE (workspace_slug, number),
	FOREIGN KEY (workspace_slug, number) REFERENCES changes ON DELETE CASCADE
)`)
		return err
	},
}

// EventRestack is the event of a change restacked onto the change below
// it, after the parley.change.version of the version the restack made.
const EventRestack = "parley.change.restack"

// TriggerVersion is the trigger of the restack a new version of the change
// below asks for.
const TriggerVersion = "version"

// TriggerLand is the trigger of the restack the landing of the change below
// asks for.
const TriggerLand = "land"

// TriggerManual is the trigger of the restack a caller asks for: an
// explicit depends_on naming a change the caller's commits do not sit on.
const TriggerManual = "manual"

// restackJob is a restack to run: the change number of the workspace, for
// the trigger, onto the commit onto when it names one, and seq, the
// pending record it runs, 0 for none. background is set for a job the
// worker runs, which a change too deep in its stack skips.
type restackJob struct {
	workspace  string
	number     int64
	seq        int64
	trigger    string
	onto       string
	background bool
}

// pendRestack records in tx, the transaction of what caused it, that the
// change ch must be restacked for the trigger, onto the commit onto when
// it is not "", replacing the restack the change had pending, so that the
// newest commit below is the one it is restacked onto. A record that
// cannot be written fails tx. The worker is woken; it runs the restack
// once tx has committed.
//
// A change deeper in its stack than the limit gets none: the limit exists
// to bound the restacks a move below costs, and the change it records
// none for gets no version that would restack the changes above it. A
// workspace with automatic restacks off gets none either; its change is
// only flagged, with auto_restack_off, when it does not sit on the commit
// it must (flagBehind).
func (c *Component) pendRestack(ctx context.Context, tx *sql.Tx, ch Change, trigger, onto string) error {
	deep, err := c.tooDeep(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil || deep {
		return err
	}
	w, found, err := c.deps.Workspaces.Workspace(ctx, tx, ch.WorkspaceSlug)
	if err != nil {
		return err
	}
	if found && !w.AutoRestack {
		return c.flagBehind(ctx, tx, ch, onto)
	}
	if _, err := tx.ExecContext(ctx, `INSERT OR REPLACE INTO change_restacks (workspace_slug, number, trigger, onto) VALUES (?, ?, ?, ?)`,
		ch.WorkspaceSlug, ch.Number, trigger, onto); err != nil {
		return fmt.Errorf("record the restack of %s: %w", ch.Ref, err)
	}
	c.restacks.wake()
	return nil
}

// flagBehind sets in tx needs_restack, for auto_restack_off, on the change
// ch when it does not sit on the commit it must, as its link now reads
// (sitOn, with onto the commit a restack would have named), so that a
// caller sees that the change is behind and restacks it. Nothing moves. A
// change whose target branch is missing has no commit to sit on and is
// left as it is.
func (c *Component) flagBehind(ctx context.Context, tx *sql.Tx, ch Change, onto string) error {
	now, err := readChange(ctx, tx, ch.WorkspaceSlug, ch.Number)
	if err != nil {
		return err
	}
	behind, err := c.behind(ctx, tx, now, onto)
	if err != nil || !behind {
		return err
	}
	return c.SetNeedsRestack(ctx, tx, ch.WorkspaceSlug, ch.Number, true, new(RestackAutoOff))
}

// behind reports whether the change ch, read through q, does not sit on
// the commit it must (sitOn, with onto as a pending record names it), as
// a restack judges it (sitsOn). Ancestry is asked of the target's
// repository, which holds every version head of its changes and every
// base they are measured from. A change with no commit to sit on is not
// behind.
func (c *Component) behind(ctx context.Context, q Querier, ch Change, onto string) (bool, error) {
	at, err := c.sitOn(ctx, q, ch, onto)
	if err != nil || at == "" {
		return false, err
	}
	repo, err := c.deps.Repos(ctx, ch.WorkspaceSlug)
	if err != nil {
		return false, err
	}
	on, err := sitsOn(ctx, repo, baseColumn(ch.BaseSHA), ch.HeadSHA, at)
	return !on, err
}

// hasPendingRestack reports whether the change number of the workspace has
// a pending restack, read through q.
func hasPendingRestack(ctx context.Context, q Querier, workspace string, number int64) (bool, error) {
	var pending bool
	err := q.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM change_restacks WHERE workspace_slug = ? AND number = ?)`, workspace, number).Scan(&pending)
	if err != nil {
		return false, fmt.Errorf("read the restack of %s: %w", Ref(workspace, number), err)
	}
	return pending, nil
}

// OnLanded is told that the change number of the workspace landed as the
// commit landed on its target branch. In a transaction of its own, since
// the landing has ended and nothing it does may undo it, each draft or
// open change directly above it no longer depends on it, keeps the landed
// commit as the commit it must sit on and gets a pending restack onto it;
// the changes further up keep their links and are restacked by the
// versions those restacks record. A change without dependants records
// nothing.
func (c *Component) OnLanded(ctx context.Context, workspace string, number int64, landed string) error {
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		deps, err := dependants(ctx, tx, workspace, number)
		if err != nil {
			return err
		}
		s := stacks{c}
		for _, n := range deps {
			if err := c.SetDependsOn(ctx, tx, workspace, n, nil); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE changes SET restack_onto = ? WHERE workspace_slug = ? AND number = ?`, landed, workspace, n); err != nil {
				return fmt.Errorf("store what %s sits on: %w", Ref(workspace, n), err)
			}
			// The depth first, so that the restack is recorded for the
			// change as it now sits in its stack.
			if err := s.recomputeDepth(ctx, tx, workspace, n); err != nil {
				return err
			}
			d, err := readChange(ctx, tx, workspace, n)
			if err != nil {
				return err
			}
			if err := c.pendRestack(ctx, tx, d, TriggerLand, landed); err != nil {
				return err
			}
		}
		return nil
	})
}

// dropRestack removes in tx the pending restack of the job, unless a newer
// one replaced it while the job ran.
func dropRestack(ctx context.Context, tx *sql.Tx, j restackJob) (bool, error) {
	res, err := tx.ExecContext(ctx, `DELETE FROM change_restacks WHERE workspace_slug = ? AND number = ? AND seq = ?`, j.workspace, j.number, j.seq)
	if err != nil {
		return false, fmt.Errorf("remove the restack of %s: %w", Ref(j.workspace, j.number), err)
	}
	n, err := res.RowsAffected()
	return n > 0, err
}

// RunRestacks runs the pending restacks, one at a time in the order they
// were recorded, until ctx ends: first those pending at the start, which
// the hub owed when it stopped, then each one recorded once the
// transaction that records it has committed. A restack that fails, or
// whose source branch keeps moving under it, is logged and stays pending
// until a newer one replaces it or the hub starts again.
func (c *Component) RunRestacks(ctx context.Context) {
	w := &c.restacks
	w.wake()
	for ctx.Err() == nil {
		changed, run := w.begin()
		if !run {
			select {
			case <-ctx.Done():
			case <-changed:
			}
			continue
		}
		c.restackPass(ctx, true)
		w.end()
	}
}

// restackPass runs the pending restacks, those they record among them,
// until none is left that has not failed in this run of the hub, and
// returns how many it ran. In the background it stops once the worker is
// held.
func (c *Component) restackPass(ctx context.Context, background bool) int {
	ran := 0
	for ctx.Err() == nil && !(background && c.restacks.isHeld()) {
		j, found, err := c.nextRestack(ctx)
		if err != nil {
			if ctx.Err() == nil {
				c.deps.Logger.Error("the pending restacks could not be read", "error", err)
			}
			break
		}
		if !found {
			break
		}
		ran++
		res, err := c.restack(ctx, j, Caller{}, nil)
		switch {
		case err != nil && ctx.Err() != nil:
		case err != nil:
			c.restacks.keep(j)
			c.deps.Logger.Error("the restack failed", "workspace", j.workspace, "number", j.number, "error", err)
		case res.outcome == restackLost:
			c.restacks.keep(j)
			c.deps.Logger.Error("the restack failed", "workspace", j.workspace, "number", j.number,
				"error", fmt.Sprintf("the source branch moved during each of %d attempts", restackAttempts))
		}
	}
	return ran
}

// nextRestack reads the first pending restack that has not failed in this
// run of the hub, in a transaction of the store, which waits for the one
// that recorded it.
func (c *Component) nextRestack(ctx context.Context) (j restackJob, found bool, err error) {
	err = c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT seq, workspace_slug, number, trigger, onto FROM change_restacks ORDER BY seq`)
		if err != nil {
			return err
		}
		defer rows.Close()
		for rows.Next() {
			if err := rows.Scan(&j.seq, &j.workspace, &j.number, &j.trigger, &j.onto); err != nil {
				return err
			}
			if !c.restacks.kept(j) {
				found, j.background = true, true
				return nil
			}
		}
		return rows.Err()
	})
	return j, found, err
}

// restackWorker is the state of the worker that runs the pending
// restacks. Its zero value is ready.
type restackWorker struct {
	mu sync.Mutex
	// signaled is set when a restack was recorded since the worker last
	// looked, running while it runs them, held once a test holds it.
	signaled, running, held bool
	// changed is closed, and replaced, when a restack is recorded.
	changed chan struct{}
	// failed are the restacks that failed in this run of the hub, by
	// change, with the seq of their record: a newer record runs again.
	failed map[mergeKey]int64

	// The seams the tests set; nil is the hub's.
	rebase     rebaseFunc
	beforeSwap func(ctx context.Context, workspace string, number int64, attempt int)
}

// rebaseFunc replays the commits of repo after base up to head on top of
// onto, as gitrepo.Repo.RebaseFrom does.
type rebaseFunc func(ctx context.Context, repo *gitrepo.Repo, base, head, onto string, at time.Time) (string, *gitrepo.ConflictReport, error)

func rebaseFrom(ctx context.Context, repo *gitrepo.Repo, base, head, onto string, at time.Time) (string, *gitrepo.ConflictReport, error) {
	return repo.RebaseFrom(ctx, base, head, onto, at)
}

// wake tells the worker that a restack was recorded.
func (w *restackWorker) wake() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.signaled = true
	if w.changed != nil {
		close(w.changed)
	}
	w.changed = make(chan struct{})
}

// begin marks the worker running when a restack was recorded since it
// last looked and it is not held; otherwise it returns what to wait on.
func (w *restackWorker) begin() (<-chan struct{}, bool) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.changed == nil {
		w.changed = make(chan struct{})
	}
	if !w.signaled || w.held {
		return w.changed, false
	}
	w.signaled, w.running = false, true
	return nil, true
}

// end marks the worker no longer running.
func (w *restackWorker) end() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.running = false
}

func (w *restackWorker) isHeld() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.held
}

// keep remembers that the job failed, so that its record is not run again
// in this run of the hub.
func (w *restackWorker) keep(j restackJob) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failed == nil {
		w.failed = map[mergeKey]int64{}
	}
	w.failed[mergeKey{j.workspace, j.number}] = j.seq
}

// kept reports whether the record of the job failed in this run of the
// hub.
func (w *restackWorker) kept(j restackJob) bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	seq, ok := w.failed[mergeKey{j.workspace, j.number}]
	return ok && seq == j.seq
}

// seams are the replay and the hook before the swap a restack runs with.
func (w *restackWorker) seams() (rebaseFunc, func(context.Context, string, int64, int)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	rebase := w.rebase
	if rebase == nil {
		rebase = rebaseFrom
	}
	return rebase, w.beforeSwap
}
