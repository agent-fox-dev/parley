package workspace

import (
	"context"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// syncRoute is POST /workspaces/:slug/sync. The scope check and then the
// access check run before the handler, so that a PAT without
// workspaces:write is refused before any lookup, a member of the
// workspace's org who does not own it is refused, and a caller who cannot
// read the workspace gets the same 404 as for a slug no workspace has. The
// route reads no body.
func (s *Service) syncRoute() server.Route {
	return server.Route{Method: http.MethodPost, Pattern: "/workspaces/:slug/sync", Scope: scopeWrite,
		Marks: map[string]string{access.MarkNeed: string(access.NeedWrite)}, Handler: s.serveSync}
}

// syncAnswer is the answer of a completed sync: the workspace object, read
// after the sync, and the result of every branch of origin.
type syncAnswer struct {
	Object
	Branches []BranchResult `json:"branches"`
}

// serveSync syncs the route's workspace when the query is valid and the
// workspace may be synced, and answers the workspace object as the sync's
// completion left it, with the caller's level, and the branches. Nothing is fetched before every check has
// passed. The sync runs on a context the request's end does not cancel, so
// that a caller who disconnects does not stop it halfway; the principal
// stays in that context's values.
func (s *Service) serveSync(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the workspace sync found no access decision")
	}
	reset, err := boolParam(r.URL.Query(), "reset_to_origin")
	if err != nil {
		return err
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
	if err := s.requireSyncable(rec); err != nil {
		return err
	}
	ctx := context.WithoutCancel(r.Context())
	// The caller is the sync event's actor; the access check has let only
	// the admin token or a user's credential through.
	p, _ := identity.PrincipalOf(ctx)
	// An error that is a *server.Error, wrapped or not, is the answer; any
	// other is answered as a failure inside the hub.
	result, err := s.Sync(ctx, slug, SyncOptions{ResetToOrigin: reset, Actor: p})
	if err != nil {
		return err
	}
	obj, err := s.RenderWorkspace(ctx, result.Workspace, d.Level)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, syncAnswer{Object: obj, Branches: result.Branches})
	return nil
}

// requireSyncable is nil when w may be synced and otherwise the refusal,
// the first failing check in this order: a task fork has no origin, then the
// workspace's state as RequireReady judges it, then its effective setting
// sync_enabled. Status comes before the setting, so that an archived
// workspace with sync disabled is refused as archived.
func (s *Service) requireSyncable(w Workspace) error {
	if w.Kind == access.KindFork {
		return server.NewError("fork_has_no_origin", http.StatusBadRequest,
			fmt.Sprintf("workspace %q is a task fork, which has no origin to sync from", w.Slug))
	}
	if err := RequireReady(w); err != nil {
		return err
	}
	if s.settings.EffectiveSettings(w.Settings, w.DefaultBranch)["sync_enabled"] == false {
		return server.NewError("sync_disabled", http.StatusBadRequest,
			fmt.Sprintf("workspace %q has sync disabled; set sync_enabled to true to sync it", w.Slug))
	}
	return nil
}
