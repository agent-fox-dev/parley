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

// The surfaces a refusal comes in on: the REST API and git.
const (
	SurfaceAPI = "api"
	SurfaceGit = "git"
)

// msgParentOnly answers a fork-bound PAT on a route of the fork's parent
// that is not a contributor route.
const msgParentOnly = "a token bound to a task fork reaches only the contributor routes of its parent"

// msgNoWorkspace answers a bound PAT on a route that names no workspace and
// is not on the allowlist.
const msgNoWorkspace = "a token bound to a workspace cannot use this route"

// Refusal is one request the binding guard refused, as it reports it.
type Refusal struct {
	// TokenID is the bound PAT's token_id and UserID its user's id.
	TokenID, UserID string
	// BoundWorkspace is the slug the PAT is bound to.
	BoundWorkspace string
	// RequestedWorkspace is the slug the request named, exactly as typed;
	// nil for a request that names no workspace.
	RequestedWorkspace *string
	// Method is the request's method, and Route the matched route's
	// pattern: on the REST API under /api/v1, such as
	// /api/v1/workspaces/:slug/tokens; on git the git route's pattern as the
	// git server passes it.
	Method, Route string
	// Status is the status the guard answered: 403 or 404, and on git
	// always 404.
	Status int
	// Surface is where the request came in: SurfaceAPI or SurfaceGit.
	Surface string
}

// RefusalReporter is told of every request the binding guard refuses, after
// the refusal is decided and before it is answered. It cannot change the
// answer. In the hub it is a RefusalLimiter.
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
// On a route that names no workspace, a bound PAT passes on the allowlist,
// with the Binding of no relation attached, from which the route obtains the
// covered set it confines its result to (CoveredSet); every other such
// route, the identity and admin routes among them, is answered 403
// token_bound, whatever scopes the token holds.
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
		if !isAllowlisted(m.Method, m.Pattern) {
			return s.refuse(r, c, m, nil, server.TokenBound(msgNoWorkspace))
		}
		attachBinding(r, Binding{TokenID: c.TokenID, Workspace: c.Workspace})
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
	s.report(r.Context(), Refusal{
		TokenID: c.TokenID, UserID: c.UserID, BoundWorkspace: c.Workspace, RequestedWorkspace: slug,
		Method: m.Method, Route: "/api/v1" + m.Pattern, Status: e.Status(), Surface: SurfaceAPI,
	})
	return e
}

// report hands f to the service's RefusalReporter, if it has one.
func (s *Service) report(ctx context.Context, f Refusal) {
	if s.deps.Refusals != nil {
		s.deps.Refusals.Refused(ctx, f)
	}
}

// GitDecision is the binding guard's decision on a git request.
type GitDecision struct {
	// Pass is true when the request may go on to git's own checks.
	Pass bool
	// Status is the status of a refusal, 404; 0 when Pass is true.
	Status int
	// Binding is what a bound PAT that passes reaches; nil for every
	// other credential and for a refusal.
	Binding *Binding
}

// GuardGit is the binding guard on git, for the git server to call after it
// has authenticated the caller c and before it checks anything else, with
// the slug of the repository the request names, the request's method and
// the matched git route's pattern. A bound PAT passes, with its Binding, on
// the bound workspace and on a fork the token created; on every other slug,
// whether or not it has a workspace and the parent of the bound fork
// included, it is refused with 404, so that on git a bound token is answered
// like a stranger. Each refusal is reported with SurfaceGit. The admin
// token, an API key and a PAT that is not bound pass with no Binding. A
// failed read of the records is a zero decision and the error.
func (s *Service) GuardGit(ctx context.Context, c server.Caller, slug, method, pattern string) (GitDecision, error) {
	caller := CallerFrom(c)
	if caller.Workspace == "" {
		return GitDecision{Pass: true}, nil
	}
	rel, err := s.RelationOf(ctx, caller, slug)
	if err != nil {
		return GitDecision{}, fmt.Errorf("binding guard on git %s %s: %w", method, pattern, err)
	}
	if rel == RelationBound || rel == RelationCreatedFork {
		return GitDecision{Pass: true, Binding: &Binding{TokenID: caller.TokenID, Workspace: caller.Workspace, Relation: rel}}, nil
	}
	s.report(ctx, Refusal{
		TokenID: caller.TokenID, UserID: caller.UserID, BoundWorkspace: caller.Workspace, RequestedWorkspace: &slug,
		Method: method, Route: pattern, Status: http.StatusNotFound, Surface: SurfaceGit,
	})
	return GitDecision{Status: http.StatusNotFound}, nil
}
