package awareness

import (
	"context"
	"database/sql"
)

// FailNextCommit makes the transaction of the next event the worker
// processes fail with err, as a store that fails at commit would.
func FailNextCommit(c *Component, err error) {
	c.beforeCommit = func() error {
		c.beforeCommit = nil
		return err
	}
}

// CanOverlap reports whether the globs a and b can match the same files.
func CanOverlap(a, b string) bool { return canOverlap(a, b) }

// InsertClaim stores the claim cl in tx.
func InsertClaim(ctx context.Context, tx *sql.Tx, cl Claim) error { return insertClaim(ctx, tx, cl) }

// ReadClaim is the claim id stored on workspace, read through q, and
// whether there is one.
func ReadClaim(ctx context.Context, q Querier, workspace, id string) (Claim, bool, error) {
	return readClaim(ctx, q, workspace, id)
}
