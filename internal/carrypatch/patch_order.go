package carrypatch

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"time"
)

// renumber gives the patches of order, every patch of one workspace that is
// not deleted, in their new order, the positions 1 to N in tx, and stamps
// updated_at with now on each patch whose position changes and on no
// other. It returns the patches as they then stand. Every change of the
// order goes through it, inside the store transaction that reads the list,
// so that the positions stay gap-free: the store runs one write
// transaction at a time, so no other change of the list falls between the
// read and the write.
func renumber(ctx context.Context, tx *sql.Tx, order []Patch, now time.Time) ([]Patch, error) {
	out := make([]Patch, len(order))
	for i, p := range order {
		pos := i + 1
		if p.Position == nil || *p.Position != pos {
			if _, err := tx.ExecContext(ctx, `UPDATE patches SET position = ?, updated_at = ? WHERE workspace_slug = ? AND id = ?`,
				pos, now.UnixNano(), p.Workspace, p.ID); err != nil {
				return nil, fmt.Errorf("move the patch %s of %s to position %d: %w", p.BranchName, p.Workspace, pos, err)
			}
			p.Position, p.UpdatedAt = &pos, now
		}
		out[i] = p
	}
	return out, nil
}

// moveTo is the list live, in position order, with the patch id taken out
// and put back at position pos, from 1; the other patches keep their order.
func moveTo(live []Patch, id string, pos int) []Patch {
	out := make([]Patch, 0, len(live))
	var moved Patch
	for _, p := range live {
		if p.ID == id {
			moved = p
			continue
		}
		out = append(out, p)
	}
	return slices.Insert(out, min(pos-1, len(out)), moved)
}
