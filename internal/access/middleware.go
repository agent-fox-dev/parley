package access

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// MarkNeed is the key of server.Route.Marks with which a workspace route
// declares its need: "read", "write" or "owner". The route names its
// workspace with the path parameter slug, as in /workspaces/:slug/....
const MarkNeed = "access.need"

// The messages of the check's refusals. The not-found answer is built from
// a constant alone, so its bytes are the same for a workspace the caller
// cannot read and for one that does not exist.
const (
	msgNotFound = "workspace not found"
	msgReadOnly = "org members have read-only access to this workspace"
)

// errNoCaller is a request at the access step whose caller is neither the
// admin token nor a user: a wiring fault, which grants nothing.
var errNoCaller = errors.New("no authenticated caller at the workspace access check")

type decisionKey struct{}

// Check is the workspace access check, an access check of the pipeline. On
// a route marked MarkNeed it decides for the caller, the route's slug and
// the declared need: a grant attaches the decision to the request, where
// the handler reads it with DecisionOf; a member refused a write or owner
// route is answered 403 forbidden; everyone else is answered 404 not_found,
// the same bytes as for a slug with no workspace. A request with no caller,
// a need other than the three, a marked route without a slug and a failed
// read are errors, which the pipeline answers 500 internal without their
// text. Routes without the mark it lets through.
//
// The request's method plays no part: a route that needs read grants a
// member with POST, PATCH, PUT or DELETE as with GET, because a member's own
// contributions (forks, changes, verdicts, claims) change state on routes
// that need read, and those routes check their own conditions.
//
// It runs in the access position, after the scope check and before the
// idempotency stage, so that a replayed create is authorised too.
func (s *Service) Check(r *http.Request) error {
	m, ok := server.MatchedRoute(r)
	if !ok {
		return nil
	}
	need, ok := m.Route.Marks[MarkNeed]
	if !ok {
		return nil
	}
	sc, ok := server.CallerOf(r.Context())
	if !ok || !sc.Admin && sc.UserID == "" {
		return errNoCaller
	}
	slug, ok := m.Params["slug"]
	if !ok {
		return fmt.Errorf("route %s %s declares a need and has no slug", m.Method, m.Pattern)
	}
	d, err := s.Decide(r.Context(), CallerFrom(sc), slug, Need(need))
	if err != nil {
		return fmt.Errorf("decide workspace access on %s %s: %w", m.Method, m.Pattern, err)
	}
	switch d.Outcome {
	case Granted:
		server.Attach(r, decisionKey{}, d)
		return nil
	case ReadOnly:
		return server.Forbidden(msgReadOnly)
	default:
		return server.NotFound(msgNotFound)
	}
}

// DecisionOf returns the decision that granted the request r access to its
// workspace: the caller's level and the workspace's record as read while
// deciding, so that a route's own condition (the fork's owner, its parent,
// its kind) reads them without a second lookup. It reports false on a route
// that declares no need, and before the access check has run.
func DecisionOf(r *http.Request) (Decision, bool) {
	v, _ := server.Attached(r, decisionKey{})
	d, ok := v.(Decision)
	return d, ok
}
