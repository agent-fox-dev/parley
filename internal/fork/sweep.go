package fork

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// SweepInterval is the time between two sweep passes after the first.
const SweepInterval = 60 * time.Second

// expiryTTL is the reason of the expiry of a fork whose expires_at has
// passed.
const expiryTTL = "ttl"

// PassResult is what one sweep pass did.
type PassResult struct {
	// Skipped is set when another pass was running, and this one did
	// nothing.
	Skipped bool
	// Removed are the slugs of the forks the pass removed, in the order it
	// removed them.
	Removed []string
	// Failed are the slugs of the forks whose removal failed; a later pass
	// tries them again.
	Failed []string
	// Stopped is set when the pass ended before it reached every due fork,
	// because its context was cancelled.
	Stopped bool
}

// RunPass runs one sweep pass at the hub's time: it removes, with cause
// expired and the reason ttl, every fork whose expires_at is at or before
// that time, in ascending slug order, whatever the state of its parent
// except one whose parent is pending, cloning or under a maintenance
// operation, which is left for a later pass. A removal that fails is logged
// and leaves the fork whole, and the pass goes on with the next fork, so
// that the next pass finds the fork again. Then it repeats every pending
// discard of a removed fork's repository.
//
// The context is checked before each fork: once it is cancelled, as at the
// hub's stop, the pass removes no further fork and ends. A removal already
// begun runs on a context the cancellation does not reach, so that its
// transaction commits or rolls back and its discard runs. A pass requested
// while another runs returns at once with Skipped set.
func (c *Component) RunPass(ctx context.Context) PassResult {
	if !c.passMu.TryLock() {
		return PassResult{Skipped: true}
	}
	defer c.passMu.Unlock()
	var res PassResult
	now := c.deps.Now()
	slugs, err := c.expiredForks(ctx, now)
	if err != nil {
		c.deps.Logger.Error("fork sweep: expired forks not read", "error", err)
		return res
	}
	due := func(f Fork) bool { return f.ExpiresAt != nil && !f.ExpiresAt.After(now) }
	for _, slug := range slugs {
		if ctx.Err() != nil {
			res.Stopped = true
			return res
		}
		err := c.remove(context.WithoutCancel(ctx), slug, removal{cause: causeExpired, expiry: expiryTTL, due: due})
		var refusal *server.Error
		switch {
		case err == nil:
			res.Removed = append(res.Removed, slug)
			c.deps.Logger.Info("fork expired", "slug", slug, "reason", expiryTTL)
		case errors.Is(err, errGone), errors.Is(err, errNotDue):
			// Removed by another removal, or given a new lifetime, since
			// the pass read it.
		case errors.As(err, &refusal) && refusal.Status() == http.StatusConflict:
			// Its parent is busy; a later pass removes it.
		default:
			res.Failed = append(res.Failed, slug)
			c.deps.Logger.Error("fork sweep: fork not removed; tried again at the next pass", "slug", slug, "error", err)
		}
	}
	if ctx.Err() != nil {
		res.Stopped = true
		return res
	}
	c.retryDiscards(ctx)
	return res
}

// expiredForks are the slugs of the forks whose expires_at is at or before
// now, in ascending order.
func (c *Component) expiredForks(ctx context.Context, now time.Time) ([]string, error) {
	rows, err := c.deps.Store.Main().QueryContext(ctx,
		`SELECT slug FROM workspaces WHERE kind = ? AND expires_at IS NOT NULL AND expires_at <= ? ORDER BY slug`,
		access.KindFork, now.UnixNano())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var slugs []string
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, err
		}
		slugs = append(slugs, slug)
	}
	return slugs, rows.Err()
}

// pendingDiscard is a recorded discard of a removed fork's repository that
// failed.
type pendingDiscard struct {
	slug, parent string
}

// retryDiscards repeats every pending discard, each under the create lock
// of the fork's parent: while no workspace record has the fork's slug it
// removes the repository and drops the record once that succeeds, and logs
// a failure, keeping the record for the next pass. Once a fork has the slug
// again, its create has replaced the leftover repository with its own,
// which must stay, so the record is dropped without a removal.
func (c *Component) retryDiscards(ctx context.Context) {
	pending, err := c.pendingDiscards(ctx)
	if err != nil {
		c.deps.Logger.Error("fork sweep: pending discards not read", "error", err)
		return
	}
	for _, p := range pending {
		if ctx.Err() != nil {
			return
		}
		if err := c.retryDiscard(ctx, p); err != nil {
			c.deps.Logger.Error("fork sweep: repository of a removed fork not removed; tried again at the next pass",
				"slug", p.slug, "error", err)
		}
	}
}

// retryDiscard repeats the pending discard p (see retryDiscards).
func (c *Component) retryDiscard(ctx context.Context, p pendingDiscard) error {
	unlock := c.lockParent(p.parent)
	defer unlock()
	var n int
	if err := c.deps.Store.Main().QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE slug = ?`, p.slug).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if err := c.deps.RemoveRepo(c.deps.Workspaces.RepoPath(p.slug)); err != nil {
			return err
		}
	}
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM fork_discards WHERE slug = ?`, p.slug)
		return err
	})
}

// pendingDiscards are the recorded pending discards, by slug.
func (c *Component) pendingDiscards(ctx context.Context) ([]pendingDiscard, error) {
	rows, err := c.deps.Store.Main().QueryContext(ctx, `SELECT slug, parent_slug FROM fork_discards ORDER BY slug`)
	if err != nil {
		return nil, fmt.Errorf("read the pending discards: %w", err)
	}
	defer rows.Close()
	var out []pendingDiscard
	for rows.Next() {
		var p pendingDiscard
		if err := rows.Scan(&p.slug, &p.parent); err != nil {
			return nil, fmt.Errorf("read the pending discards: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RunSweep runs the sweep until ctx is cancelled: one pass at once, then one
// at each tick of the ticker Deps.SweepTicks starts, every SweepInterval
// unless a test replaces it. It returns once ctx is cancelled and the pass
// running then has ended.
func (c *Component) RunSweep(ctx context.Context) {
	ticks, stop := c.deps.SweepTicks()
	defer stop()
	sweepLoop(ctx, ticks, func(ctx context.Context) { c.RunPass(ctx) })
}

// sweepLoop runs pass at once, then once per tick of ticks, until ctx is
// cancelled; a tick that arrives with the cancellation starts no pass.
func sweepLoop(ctx context.Context, ticks <-chan time.Time, pass func(context.Context)) {
	if ctx.Err() != nil {
		return
	}
	pass(ctx)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			if ctx.Err() != nil {
				return
			}
			pass(ctx)
		}
	}
}

// realTicks is the sweep's ticker when no test replaces it.
func realTicks() (<-chan time.Time, func()) {
	t := time.NewTicker(SweepInterval)
	return t.C, t.Stop
}
