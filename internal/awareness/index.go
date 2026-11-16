package awareness

import (
	"context"
	"database/sql"
	"fmt"
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

// update is what one event does to the index. It is computed before the
// event's transaction, since it reads the change and lists its files with
// git, which must not hold the store's write lock, and applied in it.
type update struct {
	// typ is the event's type.
	typ string
	// first is whether a version event records the change's first version.
	first bool
	// change is the change the event names, as the change reader read it,
	// and files the files of its current version for a version or a
	// restore.
	change Change
	files  []string
	// landed is how a land event's change landed.
	landed Landed
}

// apply writes u to the index in the event's transaction tx: a version
// replaces the change's files and target branch and puts it in flight, or
// in the phase its record says for a change that left flight since, a
// land marks it landed with its landed commit and time and keeps its
// files, an abandon takes it out of flight, a restore puts it back in
// flight with the files of its current version, in the same way. What else
// an index update causes is written here too, so that it commits with the
// update and the worker's position: the first version of a change links
// the active claims of its session to it, a land or an abandon fulfils the
// active claims linked to the change (a restore turns none back), and,
// after every kind of update, the announced pairs that include the change
// are brought up to date with its partners.
func (c *Component) apply(ctx context.Context, tx *sql.Tx, u update) error {
	ch := u.change
	now := c.deps.Now()
	var err error
	switch u.typ {
	case eventVersion, eventRestore:
		phase, landed := currentPhase(ch)
		if err = putChange(ctx, tx, ch, phase, landed); err == nil {
			err = putFiles(ctx, tx, ch.Workspace, ch.Number, u.files)
		}
		if err == nil && u.typ == eventVersion && u.first {
			err = linkClaims(ctx, tx, ch, now)
		}
	case eventLand:
		if err = putChange(ctx, tx, ch, PhaseLanded, &u.landed); err == nil {
			err = fulfilClaims(ctx, tx, ch.Workspace, ch.Number, now)
		}
	case eventAbandon:
		if err = putChange(ctx, tx, ch, PhaseOut, nil); err == nil {
			err = fulfilClaims(ctx, tx, ch.Workspace, ch.Number, now)
		}
	}
	if err != nil {
		return err
	}
	return c.announce(ctx, tx, ch)
}

// currentPhase is the phase a version or a restore of ch records, as the
// change's record says now: the worker can read the event after the change
// landed or was abandoned, and the event then must not put it back in
// flight. A landed change keeps how it landed, nil when the record does not
// say; its land event, which follows, records it then.
func currentPhase(ch Change) (string, *Landed) {
	switch {
	case inFlight(ch.State):
		return PhaseInFlight, nil
	case ch.State == stateLanded:
		return PhaseLanded, ch.Landed
	}
	return PhaseOut, nil
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

// Position is the seq of the last event the index worker is done with.
func (c *Component) Position(ctx context.Context) (int64, error) {
	if c.deps.Store == nil {
		return 0, errNotBound
	}
	var seq int64
	err := c.deps.Store.Main().QueryRowContext(ctx, `SELECT seq FROM awareness_position WHERE id = 1`).Scan(&seq)
	return seq, err
}
