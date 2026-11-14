package fork

import (
	"context"
	"database/sql"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// RemoveWithCause runs the in-transaction step of a fork's removal for the
// fork slug, with cause deleted, expired or parent_deleted, by actor, in a
// transaction of its own.
func (c *Component) RemoveWithCause(ctx context.Context, slug, cause string, actor identity.Principal) error {
	return c.deps.Transact(ctx, func(tx *sql.Tx) error {
		_, err := c.removeIn(ctx, tx, slug, removal{cause: removalCause(cause), actor: actor, expiry: "ttl"})
		return err
	})
}

// SweepLoop runs pass at once, then once per tick of ticks, until ctx is
// cancelled, as the sweep does with its passes.
func SweepLoop(ctx context.Context, ticks <-chan time.Time, pass func(context.Context)) {
	sweepLoop(ctx, ticks, pass)
}
