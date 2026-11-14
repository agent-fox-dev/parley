package workspace

import (
	"context"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/server"
)

// withMarker runs fn while the workspace slug holds the maintenance marker
// m, which RequireReady reads, so that no other request that needs the
// workspace's repository runs beside the operation. The marker is the
// record's maintenance column (see errata 14): setting it is one
// conditional write, so that of two operations that ask at once exactly one
// holds it; the other is answered 409 workspace_busy, or 404 when the
// workspace is gone, without fn running. The marker is released when fn
// returns or panics, on a context the request's end does not cancel, and
// only while it is still m. Neither write changes updated_at, since the
// marker is the hub's own and no response shows it.
func (s *Service) withMarker(ctx context.Context, slug string, m Marker, fn func() error) error {
	db := s.deps.Store.Main()
	res, err := db.ExecContext(ctx, `UPDATE workspaces SET maintenance = ? WHERE slug = ? AND maintenance = ''`, string(m), slug)
	if err != nil {
		return fmt.Errorf("set the %s marker of workspace %q: %w", m, slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("set the %s marker of workspace %q: %w", m, slug, err)
	}
	if n == 0 {
		w, found, err := BySlug(ctx, db, slug)
		switch {
		case err != nil:
			return err
		case !found:
			return server.NotFound(ErrNotFound.Error())
		case w.Marker == MarkerNone:
			// Released between the two reads: the operation that held it
			// has just ended.
			return busy(fmt.Sprintf("workspace %q is busy; try again later", slug))
		}
		return RequireReady(w)
	}
	defer func() {
		if _, err := db.ExecContext(context.WithoutCancel(ctx),
			`UPDATE workspaces SET maintenance = '' WHERE slug = ? AND maintenance = ?`, slug, string(m)); err != nil {
			s.deps.Logger.Error("workspace maintenance marker not released", "slug", slug, "marker", string(m), "error", err)
		}
	}()
	return fn()
}

// ClearMarkers releases through ex the maintenance marker of every
// workspace. The hub calls it at start, before it serves or runs clone
// work: a marker is held only while an operation runs in the process, and
// each operation changes the store in one transaction, so a marker a hub
// left when it stopped in the middle of one guards nothing.
func ClearMarkers(ctx context.Context, ex Execer) error {
	if _, err := ex.ExecContext(ctx, `UPDATE workspaces SET maintenance = '' WHERE maintenance <> ''`); err != nil {
		return fmt.Errorf("release the workspaces' maintenance markers: %w", err)
	}
	return nil
}
