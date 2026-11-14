package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// eventReclone is the event a reclone writes.
const eventReclone = "parley.workspace.reclone"

// recloneRoute is POST /workspaces/:slug/reclone. As for the archive, the
// scope check and then the access check run before the handler, so that
// only the owner and the admin token reach it. The route reads no body.
func (s *Service) recloneRoute() server.Route {
	return server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/reclone", Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: s.serveReclone}
}

// recloneEventMeta is the metadata of a reclone event: the outcome of the
// safety copy.
type recloneEventMeta struct {
	OriginPush string `json:"origin_push"`
}

// serveReclone throws away the hub's copy of the route's workspace when it
// may be recloned, puts the workspace back to pending and wakes the clone
// workers, which clone origin afresh. It answers the workspace object as
// the reclone left it, with the caller's level. Once every check has
// passed the work runs on a context the request's end does not cancel, and
// with the workspace's maintenance marker held.
func (s *Service) serveReclone(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the workspace reclone found no access decision")
	}
	slug := server.Param(r, "slug")
	rec, found, err := BySlug(r.Context(), s.deps.Store.Main(), slug)
	if err != nil {
		return err
	}
	if !found {
		return server.NotFound(ErrNotFound.Error())
	}
	if err := s.recloneRefusal(r.Context(), rec, false); err != nil {
		return err
	}
	ctx := context.WithoutCancel(r.Context())
	p, _ := identity.PrincipalOf(ctx)
	actorType, actorID, ok := eventActor(p)
	if !ok {
		return fmt.Errorf("reclone workspace %q: the reclone names no caller", slug)
	}
	var recloned Workspace
	err = s.withMarker(ctx, slug, MarkerReclone, func() error {
		var err error
		recloned, err = s.reclone(ctx, slug, actorType, actorID)
		return err
	})
	if err != nil {
		return err
	}
	obj, err := s.RenderWorkspace(ctx, recloned, d.Level)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, obj)
	return nil
}

// recloneRefusal is nil when w may be recloned and otherwise the refusal,
// the first failing check in this order: a task fork has no origin to
// clone, then the maintenance marker, then the status, which must be ready
// or failed, then the forks, which depend on the copy a reclone throws
// away. held tells that the caller holds w's marker itself.
func (s *Service) recloneRefusal(ctx context.Context, w Workspace, held bool) error {
	if w.Kind == access.KindFork {
		return server.NewError("fork_has_no_origin", http.StatusBadRequest,
			fmt.Sprintf("workspace %q is a task fork, which has no origin to reclone from", w.Slug))
	}
	if held {
		w.Marker = MarkerNone
	}
	if w.Marker != MarkerNone || (w.Status != StatusReady && w.Status != StatusFailed) {
		// The state guard answers the marker, a clone that has not finished
		// and an archived workspace as every route that needs the repository
		// does.
		return RequireReady(w)
	}
	var forks int
	if err := s.deps.Store.Main().QueryRowContext(ctx, `SELECT count(*) FROM workspaces WHERE parent_slug = ?`, w.Slug).
		Scan(&forks); err != nil {
		return fmt.Errorf("count the forks of workspace %q: %w", w.Slug, err)
	}
	if forks > 0 {
		return server.NewError("has_forks", http.StatusConflict,
			fmt.Sprintf("workspace %q has %d forks, which depend on its copy; delete them first", w.Slug, forks))
	}
	return nil
}

// reclone reclones the workspace slug, whose maintenance marker the caller
// holds, by the actor given, and returns its record as the reclone left
// it. It offers every branch to origin first, whatever origin answers, then
// in one store transaction sets the status to pending, clears sync_error and
// origin_head_sha, gives the record a new instance id and writes the
// reclone event; when the transaction fails nothing has changed.
// clone_error stays until the clone work starts. The new instance id makes the copy no longer the record's, so
// that clone work clones origin afresh even when a hub that stopped before
// the removal left the copy on disk. Once the transaction has committed the
// copy, and with it every origin head, is removed and the clone workers
// are woken. head_sha, default_branch and last_sync_at stay until the
// clone completes. No data another package keeps for the workspace is
// removed.
func (s *Service) reclone(ctx context.Context, slug, actorType string, actorID *string) (Workspace, error) {
	// Read again under the marker: an operation that ended between the
	// handler's checks and the marker may have changed the status, and a
	// fork may have been made.
	rec, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil {
		return Workspace{}, err
	}
	if !found {
		return Workspace{}, server.NotFound(ErrNotFound.Error())
	}
	if err := s.recloneRefusal(ctx, rec, true); err != nil {
		return Workspace{}, err
	}
	outcome := s.safetyCopy(ctx, rec)
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ?, sync_error = NULL, origin_head_sha = NULL,
			instance_id = ?, updated_at = ? WHERE slug = ?`,
			string(StatusPending), NewInstanceID(), s.deps.Now().UnixNano(), slug)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			return ErrNotFound
		}
		_, err = s.deps.Events.Emit(ctx, tx, event.Input{Type: eventReclone, Workspace: &slug,
			ActorType: actorType, ActorID: actorID, ResourceType: "workspace", ResourceID: slug,
			Metadata: recloneEventMeta{OriginPush: outcome}})
		return err
	})
	if err != nil {
		return Workspace{}, fmt.Errorf("reclone workspace %q: %w", slug, err)
	}
	// Read before the clone workers are woken, so that the answer is the
	// record as the reclone left it, not as a clone has already moved it.
	rec, found, err = BySlug(ctx, s.deps.Store.Main(), slug)
	if err == nil && !found {
		err = fmt.Errorf("reclone workspace %q: the record is gone after the reclone", slug)
	}
	// The record no longer owns the copy: a copy that cannot be removed now
	// is replaced by the clone work, so the failure is only logged.
	dir := s.RepoPath(slug)
	if err := s.deps.RemoveCopy(dir); err != nil {
		s.deps.Logger.Error("workspace copy not removed after a reclone", "slug", slug, "path", dir, "error", err)
	}
	s.Enqueue(slug)
	return rec, err
}
