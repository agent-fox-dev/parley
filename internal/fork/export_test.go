package fork

import (
	"context"
	"database/sql"

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
