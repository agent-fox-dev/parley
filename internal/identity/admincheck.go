package identity

import (
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// MarkAdmin is the key of server.Route.Marks that declares an admin route:
// AdminCheck lets only an admin call it. Its value is not read.
const MarkAdmin = "identity.admin"

// AdminCheck is the admin check, an access check of the pipeline. On a
// route marked MarkAdmin it passes the admin token and an API key whose
// user has the role admin, and answers every other caller, a PAT of an
// admin included, 403 forbidden; other routes it lets through. It runs in
// the access position, after the body's size and media type are checked
// and before the handler validates the body or looks anything up, so a
// refused request changes nothing. The role is the one authentication read
// from the store for this request, so a demotion holds from the next
// request on.
func (s *Service) AdminCheck(r *http.Request) error {
	m, ok := server.MatchedRoute(r)
	if !ok {
		return nil
	}
	if _, admin := m.Route.Marks[MarkAdmin]; !admin {
		return nil
	}
	if p, ok := PrincipalOf(r.Context()); ok && isAdmin(p) {
		return nil
	}
	return server.Forbidden("this route needs an admin")
}

// isAdmin reports whether p is an admin on the user and org routes: the
// admin token, or an API key whose user has the role admin. A PAT never
// is, whatever its user's role.
func isAdmin(p Principal) bool {
	return p.Type == TypeAdmin || p.Type == TypeAPIKey && p.User != nil && p.User.Role == RoleAdmin
}
