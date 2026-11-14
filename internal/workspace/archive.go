package workspace

import (
	"context"
	"database/sql"
	"fmt"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// eventArchive is the event an archive writes.
const eventArchive = "parley.workspace.archive"

// archiveRoute is POST /workspaces/:slug/archive. The scope check and then
// the access check run before the handler, as for the other routes that
// change a workspace, so that only the owner and the admin token reach it,
// and a caller who cannot read the workspace gets the same 404 as for a
// slug no workspace has. The route reads no body.
func (s *Service) archiveRoute() server.Route {
	return server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/archive", Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: s.serveArchive}
}

// archiveEventMeta is the metadata of an archive event: the workspace's
// head when it was archived, null for one whose clone failed, the outcome
// of the safety copy, and how many forks the workspace has.
type archiveEventMeta struct {
	HeadSHA    *string `json:"head_sha"`
	OriginPush string  `json:"origin_push"`
	Forks      int     `json:"forks"`
}

// serveArchive archives the route's workspace when it may be archived, and
// answers the workspace object as the archive left it, with the caller's
// level. Once every check has passed the work runs on a context the
// request's end does not cancel, so that a caller who disconnects does not
// stop it halfway, and with the workspace's maintenance marker held.
func (s *Service) serveArchive(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the workspace archive found no access decision")
	}
	slug := server.Param(r, "slug")
	rec, found, err := BySlug(r.Context(), s.deps.Store.Main(), slug)
	if err != nil {
		return err
	}
	if !found {
		// Deleted between the access check and this read: the answer is the
		// one the check gives for a slug no workspace has.
		return server.NotFound(ErrNotFound.Error())
	}
	if err := archiveRefusal(rec, false); err != nil {
		return err
	}
	ctx := context.WithoutCancel(r.Context())
	// The caller is the archive event's actor; the access check has let
	// only the admin token or a user's credential through.
	p, _ := identity.PrincipalOf(ctx)
	actorType, actorID, ok := eventActor(p)
	if !ok {
		return fmt.Errorf("archive workspace %q: the archive names no caller", slug)
	}
	var archived Workspace
	err = s.withMarker(ctx, slug, MarkerArchive, func() error {
		var err error
		archived, err = s.archive(ctx, slug, actorType, actorID)
		return err
	})
	if err != nil {
		return err
	}
	obj, err := s.RenderWorkspace(ctx, archived, d.Level)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, obj)
	return nil
}

// archiveRefusal is nil when w may be archived and otherwise the refusal,
// the first failing check in this order: a task fork is not archived on
// its own, then the maintenance marker, then the status, which must be
// ready or failed. held tells that the caller holds w's marker itself.
func archiveRefusal(w Workspace, held bool) error {
	if w.Kind == access.KindFork {
		return server.NewError("not_supported_on_fork", http.StatusBadRequest,
			fmt.Sprintf("workspace %q is a task fork, which cannot be archived; delete it instead", w.Slug))
	}
	if held {
		w.Marker = MarkerNone
	}
	if w.Marker == MarkerNone && (w.Status == StatusReady || w.Status == StatusFailed) {
		return nil
	}
	// The state guard answers the marker, a clone that has not finished and
	// an archived workspace as every route that needs the repository does.
	return RequireReady(w)
}

// archiveReason is the reason an archive gives the session closer.
const archiveReason = "workspace archived"

// archive archives the workspace slug, whose maintenance marker the caller
// holds, by the actor given, and returns its record as the archive left it.
// It offers every branch to origin first, whatever origin answers, then in
// one store transaction sets the status to archived, clears clone_error,
// writes the archive event and has the session closer end the sessions of
// the workspace and its forks; when the transaction fails nothing has
// changed. Once it has committed, every archive observer is told. The hub's
// copy, the forks and what they own are left as they are: an archived
// workspace can be reactivated from them.
func (s *Service) archive(ctx context.Context, slug, actorType string, actorID *string) (Workspace, error) {
	// Read again under the marker: an operation that ended between the
	// handler's checks and the marker may have changed the status.
	rec, found, err := BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil {
		return Workspace{}, err
	}
	if !found {
		return Workspace{}, server.NotFound(ErrNotFound.Error())
	}
	if err := archiveRefusal(rec, true); err != nil {
		return Workspace{}, err
	}
	set := s.teardown.snapshot()
	outcome := s.safetyCopy(ctx, rec)
	var forks []string
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx, `UPDATE workspaces SET status = ?, clone_error = NULL, updated_at = ? WHERE slug = ?`,
			string(StatusArchived), s.deps.Now().UnixNano(), slug)
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
		if forks, err = forksOf(ctx, tx, slug); err != nil {
			return err
		}
		if _, err := s.deps.Events.Emit(ctx, tx, event.Input{Type: eventArchive, Workspace: &slug,
			ActorType: actorType, ActorID: actorID, ResourceType: "workspace", ResourceID: slug,
			Metadata: archiveEventMeta{HeadSHA: rec.HeadSHA, OriginPush: outcome, Forks: len(forks)}}); err != nil {
			return err
		}
		return s.closeSessions(ctx, tx, set.closer, slug, forks)
	})
	if err != nil {
		return Workspace{}, fmt.Errorf("archive workspace %q: %w", slug, err)
	}
	for _, o := range set.observers {
		if err := o.WorkspaceArchived(ctx, slug, slices.Clone(forks)); err != nil {
			s.deps.Logger.Error("workspace archive observer failed", "slug", slug, "error", err)
		}
	}
	rec, found, err = BySlug(ctx, s.deps.Store.Main(), slug)
	if err != nil {
		return Workspace{}, err
	}
	if !found {
		return Workspace{}, fmt.Errorf("archive workspace %q: the record is gone after the archive", slug)
	}
	return rec, nil
}

// forksOf returns the slugs of the forks of the workspace parent, in
// ascending order, empty and not nil when it has none.
func forksOf(ctx context.Context, tx *sql.Tx, parent string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT slug FROM workspaces WHERE parent_slug = ? ORDER BY slug`, parent)
	if err != nil {
		return nil, fmt.Errorf("list the forks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	forks := []string{}
	for rows.Next() {
		var slug string
		if err := rows.Scan(&slug); err != nil {
			return nil, fmt.Errorf("list the forks: %w", err)
		}
		forks = append(forks, slug)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the forks: %w", err)
	}
	return forks, nil
}

// closeSessions has closer, when there is one, end in the archive's
// transaction tx the sessions of the workspace slug and of its forks. It
// runs under a savepoint: a closer that fails has its own writes undone and
// its error logged, and the archive goes on, since the workspace is
// archived whether or not a session could be ended. Only a failure of the
// savepoint itself is returned.
func (s *Service) closeSessions(ctx context.Context, tx *sql.Tx, closer SessionCloser, slug string, forks []string) error {
	if closer == nil {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT archive_sessions`); err != nil {
		return fmt.Errorf("open the sessions' savepoint: %w", err)
	}
	if err := closer.CloseSessions(ctx, tx, append([]string{slug}, forks...), archiveReason); err != nil {
		s.deps.Logger.Error("workspace sessions not closed on archive", "slug", slug, "error", err)
		if _, err := tx.ExecContext(ctx, `ROLLBACK TO archive_sessions`); err != nil {
			return fmt.Errorf("undo the session closer's writes: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `RELEASE archive_sessions`); err != nil {
		return fmt.Errorf("release the sessions' savepoint: %w", err)
	}
	return nil
}
