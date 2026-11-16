package awareness

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// The event types the index follows; the worker skips every other one.
// A retarget is a version event too.
const (
	eventVersion = "parley.change.version"
	eventLand    = "parley.change.land"
	eventAbandon = "parley.change.abandon"
	eventRestore = "parley.change.restore"
)

// follows reports whether the index acts on events of type typ.
func follows(typ string) bool {
	switch typ {
	case eventVersion, eventLand, eventAbandon, eventRestore:
		return true
	}
	return false
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

// update is what one event does to the index. It is computed before the
// event's transaction, since it reads the change and lists its files with
// git, which must not hold the store's write lock, and applied in it.
type update struct {
	// seq and typ are the event's.
	seq int64
	typ string
	// change is the change the event names, as the change reader read it,
	// and files the files of its current version for a version or a
	// restore.
	change Change
	files  []string
	// landed is how a land event's change landed.
	landed Landed
}

// apply writes u to the index in the event's transaction tx: a version
// replaces the change's files and target branch and puts it in flight, a
// land marks it landed with its landed commit and time and keeps its
// files, an abandon takes it out of flight, a restore puts it back in
// flight with the files of its current version. What else an index update
// causes is written here too, so that it commits with the update and the
// worker's position: after every kind of update, the announced pairs that
// include the change are brought up to date with its partners.
func (c *Component) apply(ctx context.Context, tx *sql.Tx, u update) error {
	ch := u.change
	var err error
	switch u.typ {
	case eventVersion, eventRestore:
		if err = putChange(ctx, tx, ch, PhaseInFlight, nil); err == nil {
			err = putFiles(ctx, tx, ch.Workspace, ch.Number, u.files)
		}
	case eventLand:
		err = putChange(ctx, tx, ch, PhaseLanded, &u.landed)
	case eventAbandon:
		err = putChange(ctx, tx, ch, PhaseOut, nil)
	}
	if err != nil {
		return err
	}
	return c.announce(ctx, tx, ch)
}

// putChange records ch in the index under its current target branch, in
// phase, landed as landed (nil while it has not).
func putChange(ctx context.Context, tx *sql.Tx, ch Change, phase string, landed *Landed) error {
	var sha *string
	var at *int64
	if landed != nil {
		sha, at = &landed.SHA, new(landed.At.UnixNano())
	}
	_, err := tx.ExecContext(ctx, `INSERT INTO awareness_changes
	(workspace_slug, number, target_branch, phase, landed_sha, landed_at, session_id) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT (workspace_slug, number) DO UPDATE SET target_branch = excluded.target_branch, phase = excluded.phase,
	landed_sha = excluded.landed_sha, landed_at = excluded.landed_at, session_id = excluded.session_id`,
		ch.Workspace, ch.Number, ch.TargetBranch, phase, sha, at, ch.SessionID)
	if err != nil {
		return fmt.Errorf("index %s#%d: %w", ch.Workspace, ch.Number, err)
	}
	return nil
}

// putFiles makes files the set of files of the change number of
// workspace, in place of the set recorded before.
func putFiles(ctx context.Context, tx *sql.Tx, workspace string, number int64, files []string) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM awareness_files WHERE workspace_slug = ? AND number = ?`, workspace, number); err != nil {
		return fmt.Errorf("index the files of %s#%d: %w", workspace, number, err)
	}
	if len(files) == 0 {
		return nil
	}
	stmt, err := tx.PrepareContext(ctx, `INSERT INTO awareness_files (workspace_slug, number, path) VALUES (?, ?, ?)`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, path := range files {
		if _, err := stmt.ExecContext(ctx, workspace, number, path); err != nil {
			return fmt.Errorf("index the files of %s#%d: %w", workspace, number, err)
		}
	}
	return nil
}

// setPosition stores seq as the worker's position, which only moves
// forward.
func setPosition(ctx context.Context, tx *sql.Tx, seq int64) error {
	_, err := tx.ExecContext(ctx, `UPDATE awareness_position SET seq = ? WHERE id = 1 AND seq < ?`, seq, seq)
	return err
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

// Position is the seq of the last event the index worker is done with.
func (c *Component) Position(ctx context.Context) (int64, error) {
	if c.deps.Store == nil {
		return 0, errNotBound
	}
	var seq int64
	err := c.deps.Store.Main().QueryRowContext(ctx, `SELECT seq FROM awareness_position WHERE id = 1`).Scan(&seq)
	return seq, err
}
