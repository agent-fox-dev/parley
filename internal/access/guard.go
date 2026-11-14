package access

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
)

// workspaceRoute is the route pattern, relative to /api/v1, under which a
// route names a workspace by its path parameter slug.
const workspaceRoute = "/workspaces/:slug"

// SurfaceAPI is the surface of a refusal on the REST API.
const SurfaceAPI = "api"

// msgParentOnly answers a fork-bound PAT on a route of the fork's parent
// that is not a contributor route.
const msgParentOnly = "a token bound to a task fork reaches only the contributor routes of its parent"

// Refusal is one request the binding guard refused, as it reports it.
type Refusal struct {
	// TokenID is the bound PAT's token_id and UserID its user's id.
	TokenID, UserID string
	// BoundWorkspace is the slug the PAT is bound to.
	BoundWorkspace string
	// RequestedWorkspace is the slug the request named, exactly as typed;
	// nil for a request that names no workspace.
	RequestedWorkspace *string
	// Method is the request's method, and Route the matched route's pattern
	// under /api/v1, such as /api/v1/workspaces/:slug/tokens.
	Method, Route string
	// Status is the status the guard answered: 403 or 404.
	Status int
	// Surface is where the request came in: SurfaceAPI.
	Surface string
}

// RefusalReporter is told of every request the binding guard refuses, after
// the refusal is decided and before it is answered. It cannot change the
// answer.
type RefusalReporter interface {
	Refused(ctx context.Context, f Refusal)
}

// Guard is the binding guard of the pipeline, which runs after the route's
// feature check and before the scope check. It does nothing for the admin
// token, an API key and a PAT that is not bound. For a bound PAT on a route
// under /workspaces/:slug it decides from how the slug, as typed, stands to
// the binding (RelationOf):
//
//   - the bound workspace and a fork the token created pass, with the
//     Binding attached for the handler (BindingOf);
//   - the parent of the fork the token is bound to passes on a contributor
//     route (ContributorRoutes), with the Binding attached, and is answered
//     403 token_bound on every other route;
//   - every other slug, whether or not it has a workspace and whatever the
//     user may do on it, is answered 404 not_found with the same bytes as
//     the access check's answer for a workspace the caller cannot read, so
//     a bound token cannot tell which workspaces exist.
//
// On a route that names no workspace it decides nothing.
//
// A request it passes goes on to the scope check and the access checks
// unchanged, so a binding takes access away and never grants it. Each
// refusal is reported to the service's RefusalReporter. A failed read of
// the records is an error, which the pipeline answers 500 internal.
func (s *Service) Guard(r *http.Request) error {
	sc, ok := server.CallerOf(r.Context())
	if !ok {
		return nil
	}
	c := CallerFrom(sc)
	m, matched := server.MatchedRoute(r)
	if c.Workspace == "" || !matched {
		return nil
	}
	rest, named := strings.CutPrefix(m.Pattern, workspaceRoute)
	if !named || rest != "" && !strings.HasPrefix(rest, "/") {
		return nil
	}
	slug := m.Params["slug"]
	rel, err := s.RelationOf(r.Context(), c, slug)
	if err != nil {
		return fmt.Errorf("binding guard on %s %s: %w", m.Method, m.Pattern, err)
	}
	switch {
	case rel == RelationBound || rel == RelationCreatedFork:
	case rel == RelationParentOfBoundFork && isContributorRoute(m.Method, rest):
	case rel == RelationParentOfBoundFork:
		return s.refuse(r, c, m, &slug, server.TokenBound(msgParentOnly))
	default:
		return s.refuse(r, c, m, &slug, server.NotFound(msgNotFound))
	}
	attachBinding(r, Binding{TokenID: c.TokenID, Workspace: c.Workspace, Relation: rel})
	return nil
}

// refuse reports the refusal e of the request r, which matched m and named
// the workspace slug (nil for none), and returns e as the answer.
func (s *Service) refuse(r *http.Request, c Caller, m server.Matched, slug *string, e *server.Error) error {
	if s.deps.Refusals != nil {
		s.deps.Refusals.Refused(r.Context(), Refusal{
			TokenID: c.TokenID, UserID: c.UserID, BoundWorkspace: c.Workspace, RequestedWorkspace: slug,
			Method: m.Method, Route: "/api/v1" + m.Pattern, Status: e.Status(), Surface: SurfaceAPI,
		})
	}
	return e
}
