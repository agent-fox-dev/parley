package awareness

import (
	"context"
	"database/sql"
	"errors"
	"sync/atomic"
	"time"
)

// The states of a change the index takes out of flight.
const (
	StateLanded    = stateLanded
	StateAbandoned = stateAbandoned
)

// FailNextCommit makes the transaction of the next event the worker
// processes fail with err, as a store that fails at commit would.
func FailNextCommit(c *Component, err error) {
	c.beforeCommit = func() error {
		c.beforeCommit = nil
		return err
	}
}

// PassRunning reports whether a pass of the index worker of c is running.
func PassRunning(c *Component) bool {
	if c.pass.TryLock() {
		c.pass.Unlock()
		return false
	}
	return true
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

// CountAncestryChecks counts, from now on, each check of an activity of c
// whether its since commit reaches a landed commit.
func CountAncestryChecks(c *Component) *atomic.Int64 {
	n := &atomic.Int64{}
	c.ancestryChecked = func() { n.Add(1) }
	return n
}

// IndexedChange is a change as the index holds it.
type IndexedChange struct {
	Workspace    string
	Number       int64
	TargetBranch string
	// Phase is PhaseInFlight, PhaseLanded or PhaseOut.
	Phase string
	// LandedSHA and LandedAt are the landed commit and the landing's time,
	// nil while the change has not landed.
	LandedSHA *string
	LandedAt  *time.Time
	SessionID *string
}

// LandedChange is a landed change of the index: its number, its landed
// commit and when it landed.
type LandedChange struct {
	Number int64
	SHA    string
	At     time.Time
}

// Indexed reads the change number of workspace from the index, through q
// (the caller's transaction, or nil for the store); found is false for a
// change the index does not hold.
func (c *Component) Indexed(ctx context.Context, q Querier, workspace string, number int64) (ch IndexedChange, found bool, err error) {
	var at *int64
	ch = IndexedChange{Workspace: workspace, Number: number}
	err = c.querier(q).QueryRowContext(ctx, `SELECT target_branch, phase, landed_sha, landed_at, session_id
FROM awareness_changes WHERE workspace_slug = ? AND number = ?`, workspace, number).
		Scan(&ch.TargetBranch, &ch.Phase, &ch.LandedSHA, &at, &ch.SessionID)
	if errors.Is(err, sql.ErrNoRows) {
		return IndexedChange{}, false, nil
	}
	if err != nil {
		return IndexedChange{}, false, err
	}
	if at != nil {
		ch.LandedAt = new(time.Unix(0, *at).UTC())
	}
	return ch, true, nil
}

// InFlight are the numbers of the in-flight changes of workspace with the
// target branch target, ascending, read through q.
func (c *Component) InFlight(ctx context.Context, q Querier, workspace, target string) ([]int64, error) {
	rows, err := c.querier(q).QueryContext(ctx, `SELECT number FROM awareness_changes
WHERE workspace_slug = ? AND target_branch = ? AND phase = 'in_flight' ORDER BY number`, workspace, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var n int64
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Files are the files the index holds for the change number of workspace,
// in ascending byte order, read through q; none for a change it does not
// hold.
func (c *Component) Files(ctx context.Context, q Querier, workspace string, number int64) ([]string, error) {
	rows, err := c.querier(q).QueryContext(ctx, `SELECT path FROM awareness_files
WHERE workspace_slug = ? AND number = ? ORDER BY path`, workspace, number)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Landed are the landed changes of workspace on the target branch target,
// in the order they landed, read through q.
func (c *Component) Landed(ctx context.Context, q Querier, workspace, target string) ([]LandedChange, error) {
	rows, err := c.querier(q).QueryContext(ctx, `SELECT number, landed_sha, landed_at FROM awareness_changes
WHERE workspace_slug = ? AND target_branch = ? AND phase = 'landed' ORDER BY landed_at, number`, workspace, target)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LandedChange{}
	for rows.Next() {
		var l LandedChange
		var sha sql.NullString
		var at sql.NullInt64
		if err := rows.Scan(&l.Number, &sha, &at); err != nil {
			return nil, err
		}
		l.SHA, l.At = sha.String, time.Unix(0, at.Int64).UTC()
		out = append(out, l)
	}
	return out, rows.Err()
}
