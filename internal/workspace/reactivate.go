package workspace

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// eventReactivate is the event a reactivate writes.
const eventReactivate = "parley.workspace.reactivate"

// reactivateRoute is POST /workspaces/:slug/reactivate. As for the archive,
// the scope check and then the access check run before the handler, so that
// only the owner and the admin token reach it. The route reads no body.
func (s *Service) reactivateRoute() server.Route {
	return server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/reactivate", Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: s.serveReactivate}
}

// errReactivateLost tells that the conditional write of a reactivate found
// the workspace no longer archived or with its marker set.
var errReactivateLost = errors.New("the workspace changed before the reactivate")

// serveReactivate puts the route's archived workspace back to pending and
// wakes the clone workers, which bring it to ready from the copy the
// archive kept, or from a fresh clone when that copy is no longer valid.
// It answers the workspace object as the reactivate left it, with the
// caller's level. It holds no maintenance marker: it only writes the
// record, and its write is conditional on the status and on no marker
// being set, so that it cannot slip in beside an operation that holds one.
func (s *Service) serveReactivate(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the workspace reactivate found no access decision")
	}
	slug := server.Param(r, "slug")
	rec, found, err := BySlug(r.Context(), s.deps.Store.Main(), slug)
	if err != nil {
		return err
	}
	if !found {
		return server.NotFound(ErrNotFound.Error())
	}
	if err := reactivateRefusal(rec); err != nil {
		return err
	}
	ctx := context.WithoutCancel(r.Context())
	p, _ := identity.PrincipalOf(ctx)
	actorType, actorID, ok := eventActor(p)
	if !ok {
		return fmt.Errorf("reactivate workspace %q: the reactivate names no caller", slug)
	}
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ?, clone_error = NULL, updated_at = ?
			WHERE slug = ? AND status = ? AND maintenance = ''`,
			string(StatusPending), s.deps.Now().UnixNano(), slug, string(StatusArchived))
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return errReactivateLost
		}
		_, err = s.deps.Events.Emit(ctx, tx, event.Input{Type: eventReactivate, Workspace: &slug,
			ActorType: actorType, ActorID: actorID, ResourceType: "workspace", ResourceID: slug,
			Metadata: struct{}{}})
		return err
	})
	if errors.Is(err, errReactivateLost) {
		return s.lostReactivate(ctx, slug)
	}
	if err != nil {
		return fmt.Errorf("reactivate workspace %q: %w", slug, err)
	}
	s.Enqueue(slug)
	rec, found, err = BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil {
		return err
	}
	if !found {
		return server.NotFound(ErrNotFound.Error())
	}
	obj, err := s.RenderWorkspace(ctx, rec, d.Level)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, obj)
	return nil
}

// reactivateRefusal is nil when w may be reactivated and otherwise the
// refusal, the first failing check in this order: a task fork is not
// reactivated on its own, then the maintenance marker, then the status,
// which must be archived.
func reactivateRefusal(w Workspace) error {
	switch {
	case w.Kind == access.KindFork:
		return server.NewError("not_supported_on_fork", http.StatusBadRequest,
			fmt.Sprintf("workspace %q is a task fork, which cannot be reactivated on its own", w.Slug))
	case w.Marker != MarkerNone:
		// The state guard's answer, as on every route that needs the
		// repository.
		return RequireReady(w)
	case w.Status != StatusArchived:
		return server.NewError("workspace_not_archived", http.StatusConflict,
			fmt.Sprintf("workspace %q is %s, not archived", w.Slug, w.Status))
	}
	return nil
}

// lostReactivate answers a reactivate whose write found the workspace slug
// changed since its checks, as the record read now says.
func (s *Service) lostReactivate(ctx context.Context, slug string) error {
	rec, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	switch {
	case err != nil:
		return err
	case !found:
		return server.NotFound(ErrNotFound.Error())
	}
	if err := reactivateRefusal(rec); err != nil {
		return err
	}
	// Archived again with no marker: an operation held it at the write
	// and has just ended.
	return busy(fmt.Sprintf("workspace %q is busy; try again later", slug))
}
