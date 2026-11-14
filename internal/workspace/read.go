package workspace

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/access"
	"github.com/agent-fox-dev/parley/internal/server"
)

// scopeRead is the PAT scope the workspace reads need; a PAT that holds
// workspaces:write satisfies it too.
const scopeRead = "workspaces:read"

// readRoute is GET /workspaces/:slug. The scope check and then the access
// check run before the handler, so that a PAT without the scope is refused
// before any lookup and a caller who cannot read the workspace gets the same
// 404 as for a slug no workspace has. It answers in every status and for
// both kinds.
func (s *Service) readRoute() server.Route {
	return server.Route{Method: http.MethodGet, Pattern: "/workspaces/:slug", Scope: scopeRead,
		Marks: map[string]string{access.MarkNeed: string(access.NeedRead)}, Handler: s.read}
}

// read answers the workspace object of the route's workspace, with the
// caller's level as the access check decided it.
func (s *Service) read(w http.ResponseWriter, r *http.Request) error {
	d, ok := access.DecisionOf(r)
	if !ok {
		return server.Internal("the workspace read found no access decision")
	}
	rec, found, err := BySlug(r.Context(), s.deps.Store.Main(), server.Param(r, "slug"))
	if err != nil {
		return err
	}
	if !found {
		// Deleted between the access check and this read: the answer is the
		// one the check gives for a slug no workspace has.
		return server.NotFound(ErrNotFound.Error())
	}
	obj, err := s.RenderWorkspace(r.Context(), rec, d.Level)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, obj)
	return nil
}
