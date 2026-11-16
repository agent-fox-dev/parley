package awareness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/event"
)

// EventClaimExpire is the event of a claim's expiry.
const EventClaimExpire = "parley.claim.expire"

// RunClaimExpiry is the claim expiry: a run every Deps.ExpiryInterval until
// ctx is done. A run that fails is logged, and the claims it did not expire
// are expired by the next one.
func (c *Component) RunClaimExpiry(ctx context.Context) {
	tick := time.NewTicker(c.deps.ExpiryInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		if err := c.ExpireClaims(ctx); err != nil && ctx.Err() == nil {
			c.deps.Logger.Warn("the claim expiry run failed", "error", err)
		}
	}
}

// ExpireClaims is one run of the claim expiry: every claim stored as active
// whose expires_at is not after the clock's time is stored as expired,
// ended at its expires_at, and announced with one parley.claim.expire by
// the system, in a transaction of its own. A claim whose transaction fails
// is left active for the next run, and the others are still expired; the
// error names each failure.
func (c *Component) ExpireClaims(ctx context.Context) error {
	if c.deps.Store == nil {
		return errNotBound
	}
	now := c.deps.Now()
	ids, err := dueClaims(ctx, c.deps.Store.Main(), now)
	if err != nil {
		return err
	}
	var errs []error
	for _, id := range ids {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err := c.deps.Store.Tx(ctx, func(tx *sql.Tx) error { return c.expireClaim(ctx, tx, id, now) }); err != nil {
			errs = append(errs, fmt.Errorf("expire the claim %s: %w", id, err))
		}
	}
	return errors.Join(errs...)
}

// dueClaims are the ids of the claims stored as active whose expires_at is
// not after now, the earliest first, read through q.
func dueClaims(ctx context.Context, q Querier, now time.Time) ([]string, error) {
	rows, err := q.QueryContext(ctx, `SELECT id FROM awareness_claims WHERE state = 'active' AND expires_at <= ?
ORDER BY expires_at, id`, now.UnixNano())
	if err != nil {
		return nil, fmt.Errorf("read the claims due to expire: %w", err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// expireClaim stores, in tx, the claim id as expired and writes its
// parley.claim.expire, when it is still stored as active and due at now;
// a claim released or renewed since it was read is left alone.
func (c *Component) expireClaim(ctx context.Context, tx *sql.Tx, id string, now time.Time) error {
	cl, err := scanClaim(tx.QueryRowContext(ctx, `SELECT `+claimColumns+` FROM awareness_claims
WHERE id = ? AND state = 'active' AND expires_at <= ?`, id, now.UnixNano()).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE awareness_claims SET state = 'expired', ended_at = expires_at WHERE id = ?`, id); err != nil {
		return err
	}
	meta := map[string]any{}
	event.PathList(meta, "paths", cl.Paths)
	_, err = c.deps.Emitter.Emit(ctx, tx, event.Input{
		Type: EventClaimExpire, Workspace: &cl.Workspace, ActorType: event.ActorSystem,
		ResourceType: "claim", ResourceID: cl.ID, SessionID: cl.SessionID, Metadata: meta,
	})
	return err
}
