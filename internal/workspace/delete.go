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

// eventDelete is the event a delete writes for each workspace it removes.
const eventDelete = "parley.workspace.delete"

// deleteReason is the reason a delete gives the session closer.
const deleteReason = "workspace deleted"

// deleteRoute is DELETE /workspaces/:slug, for a repository workspace and
// for a task fork. As for the archive, the scope check and then the access
// check run before the handler, so that only a caller with write access
// reaches it. The route reads no body.
func (s *Service) deleteRoute() server.Route {
	return server.Route{Method: http.MethodDelete, Pattern: "/workspaces/:slug", Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: s.serveDelete}
}

// deleteEventMeta is the metadata of a delete event: the kind of the
// removed workspace, a fork's parent (null for a repository workspace), the
// ids of the tokens revoked with it, and on the repository workspace's
// event the slugs of the forks removed with it.
type deleteEventMeta struct {
	Kind          string    `json:"kind"`
	ParentSlug    *string   `json:"parent_slug"`
	RevokedTokens []string  `json:"revoked_tokens"`
	DeletedForks  *[]string `json:"deleted_forks,omitempty"`
}

// serveDelete deletes the route's archived repository workspace with its
// forks and everything they own, and answers 204 with no body; a task fork
// it hands to the fork remover. Once every
// check has passed the work runs on a context the request's end does not
// cancel, and with the workspace's maintenance marker held.
func (s *Service) serveDelete(w http.ResponseWriter, r *http.Request) error {
	slug := server.Param(r, "slug")
	rec, found, err := BySlug(r.Context(), s.deps.Store.Main(), slug)
	if err != nil {
		return err
	}
	if !found {
		return server.NotFound(ErrNotFound.Error())
	}
	if rec.Kind == access.KindFork {
		return s.deleteFork(w, r, slug)
	}
	if err := deleteRefusal(rec, false); err != nil {
		return err
	}
	ctx := context.WithoutCancel(r.Context())
	p, _ := identity.PrincipalOf(ctx)
	actorType, actorID, ok := eventActor(p)
	if !ok {
		return fmt.Errorf("delete workspace %q: the delete names no caller", slug)
	}
	err = s.withMarker(ctx, slug, MarkerDelete, func() error {
		return s.deleteWorkspace(ctx, slug, p, actorType, actorID)
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// deleteFork hands the delete of the task fork slug, asked by the route's
// caller, to the fork remover, the one place forks are removed, since a
// fork's delete has rules of its own, and answers 204 with no body when it
// has removed the fork. Its API error is the answer otherwise, unchanged.
// With no fork remover registered nothing can remove the fork: the answer
// is 500 and nothing changes. As for a repository workspace, the delete
// runs on a context the request's end does not cancel.
func (s *Service) deleteFork(w http.ResponseWriter, r *http.Request, slug string) error {
	forks := s.teardown.snapshot().forks
	if forks == nil {
		return server.Internal(fmt.Sprintf("workspace %q is a task fork, and no fork remover deletes it", slug))
	}
	ctx := context.WithoutCancel(r.Context())
	p, _ := identity.PrincipalOf(ctx)
	if err := forks.DeleteFork(ctx, slug, p); err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// deleteRefusal is nil when the repository workspace w may be deleted and
// otherwise the refusal, the first failing check in this order: the
// maintenance marker, then the status, which must be archived. held tells
// that the caller holds w's marker itself.
func deleteRefusal(w Workspace, held bool) error {
	if held {
		w.Marker = MarkerNone
	}
	switch {
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

// deleteWorkspace deletes the repository workspace slug, whose maintenance
// marker the caller holds, by the caller p, as actor. It has the sandbox
// remover delete the sandboxes of the workspace and its forks before it
// changes anything; its error stops the delete. Then, in one store
// transaction, it removes the forks, through the fork remover when one is
// registered and otherwise one by one in ascending slug order, and then
// the workspace, each with what it owns, its record and its delete event;
// when the transaction fails nothing has changed and no repository data is
// discarded. Once it has committed, the copies of the workspaces it removed
// are discarded, then the forks' data through the fork remover's discard
// function; a copy that cannot be removed is logged, since the workspace
// is gone. No event of the log is removed.
func (s *Service) deleteWorkspace(ctx context.Context, slug string, p identity.Principal, actorType string, actorID *string) error {
	db := s.deps.Store.Main()
	// Read again under the marker: an operation that ended between the
	// handler's checks and the marker may have changed the status.
	rec, found, err := BySlug(ctx, db, slug)
	if err != nil {
		return err
	}
	if !found {
		return server.NotFound(ErrNotFound.Error())
	}
	if err := deleteRefusal(rec, true); err != nil {
		return err
	}
	set := s.teardown.snapshot()
	if set.sandboxes != nil {
		forks, err := forksOf(ctx, db, slug)
		if err != nil {
			return fmt.Errorf("delete workspace %q: %w", slug, err)
		}
		// Sandboxes live outside the store's transaction: they go first, so
		// that no record is removed while a sandbox of it still runs.
		if err := set.sandboxes.RemoveSandboxes(ctx, append([]string{slug}, forks...)); err != nil {
			return fmt.Errorf("delete workspace %q: delete the sandboxes: %w", slug, err)
		}
	}
	// removed are the forks this transaction removed itself, whose copies
	// it discards; discard is the fork remover's.
	var removed []string
	var discard func()
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		var deleted []string
		if set.forks != nil {
			slugs, fn, err := set.forks.RemoveForksOf(ctx, tx, slug, p)
			if err != nil {
				return fmt.Errorf("remove the forks: %w", err)
			}
			deleted, discard = slices.Sorted(slices.Values(slugs)), fn
		} else {
			forks, err := forksOf(ctx, tx, slug)
			if err != nil {
				return err
			}
			for _, f := range forks {
				if err := s.removeWorkspace(ctx, tx, f, &slug, actorType, actorID, nil); err != nil {
					return err
				}
			}
			deleted, removed = forks, forks
		}
		if deleted == nil {
			deleted = []string{}
		}
		return s.removeWorkspace(ctx, tx, slug, nil, actorType, actorID, &deleted)
	})
	if err != nil {
		return fmt.Errorf("delete workspace %q: %w", slug, err)
	}
	for _, ws := range append(removed, slug) {
		dir := s.RepoPath(ws)
		if err := s.deps.RemoveCopy(dir); err != nil {
			s.deps.Logger.Error("workspace copy not removed after a delete", "slug", ws, "path", dir, "error", err)
		}
	}
	if discard != nil {
		discard()
	}
	return nil
}

// removeWorkspace removes, in the transaction tx of a delete, the
// workspace slug: what it owns through RemoveOwned, then its record, with
// its settings, then it writes its delete event by the actor given. parent
// is a fork's parent, nil for a repository workspace, whose event alone
// carries deletedForks.
func (s *Service) removeWorkspace(ctx context.Context, tx *sql.Tx, slug string, parent *string,
	actorType string, actorID *string, deletedForks *[]string) error {
	revoked, err := s.RemoveOwned(ctx, tx, slug, deleteReason)
	if err != nil {
		return err
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM workspaces WHERE slug = ?`, slug)
	if err != nil {
		return fmt.Errorf("remove the record of workspace %q: %w", slug, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("remove the record of workspace %q: %w", slug, err)
	}
	if n == 0 {
		return fmt.Errorf("remove the record of workspace %q: %w", slug, ErrNotFound)
	}
	in := event.Input{Type: eventDelete, Workspace: &slug, ActorType: actorType, ActorID: actorID,
		ResourceType: "workspace", ResourceID: slug,
		Metadata: deleteEventMeta{Kind: access.KindRepository, ParentSlug: parent, RevokedTokens: revoked,
			DeletedForks: deletedForks}}
	if parent != nil {
		// The fork's record is gone already: the event component cannot
		// read its parent from it, and a fork's event must stay visible to
		// the parent's readers.
		in.Metadata = deleteEventMeta{Kind: access.KindFork, ParentSlug: parent, RevokedTokens: revoked}
		in.Parent = *parent
	}
	if _, err := s.deps.Events.Emit(ctx, tx, in); err != nil {
		return fmt.Errorf("write the delete event of workspace %q: %w", slug, err)
	}
	return nil
}
