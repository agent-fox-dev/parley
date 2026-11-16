package awareness

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// EndedClaimRetention is how long a claim is kept after it was released,
// expired or fulfilled.
const EndedClaimRetention = 7 * 24 * time.Hour

// RemoveEndedClaims is the claim cleanup: it removes every claim that was
// released, expired or fulfilled EndedClaimRetention before the clock's
// time or earlier. A claim stored as active is kept, also one long past its
// expires_at: the expiry run stores it as expired first, and it is removed
// a retention after that.
func (c *Component) RemoveEndedClaims(ctx context.Context) error {
	if c.deps.Store == nil {
		return errNotBound
	}
	cutoff := c.deps.Now().Add(-EndedClaimRetention)
	return c.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM awareness_claims
WHERE state IN ('released', 'expired', 'fulfilled') AND ended_at <= ?`, cutoff.UnixNano())
		if err != nil {
			return fmt.Errorf("remove the claims that ended by %s: %w", cutoff.Format(time.RFC3339), err)
		}
		return nil
	})
}

// RemoveWorkspace removes, in the transaction tx of a workspace's delete,
// everything the awareness holds of the workspace slug: its claims, the
// changes the index holds of it, their files and its announced pairs. It
// revokes no token. The events of the deleted workspace's changes that the
// worker has not read yet are skipped, since the change reader no longer
// finds their changes.
func RemoveWorkspace(ctx context.Context, tx *sql.Tx, slug string) error {
	for _, table := range []string{"awareness_claims", "awareness_changes", "awareness_files", "awareness_pairs"} {
		if _, err := tx.ExecContext(ctx, `DELETE FROM `+table+` WHERE workspace_slug = ?`, slug); err != nil {
			return fmt.Errorf("remove the awareness of %s from %s: %w", slug, table, err)
		}
	}
	return nil
}
