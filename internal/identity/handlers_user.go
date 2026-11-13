package identity

import (
	"database/sql"
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// needsUserMark declares a route as needing a user: the authenticator
// answers the admin token, which has none, 403 forbidden there.
var needsUserMark = map[string]string{MarkNeedsUser: "true"}

// ownUserRoutes are the routes of the caller's own profile.
func (s *Service) ownUserRoutes() []server.Route {
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/user", Scope: "users:read", Marks: needsUserMark, Handler: s.getOwnUser},
		{Method: http.MethodPatch, Pattern: "/user", Scope: "users:write", Marks: needsUserMark, Handler: s.updateOwnUser},
		{Method: http.MethodGet, Pattern: "/user/orgs", Scope: "orgs:read", Marks: needsUserMark, Handler: s.listOwnOrgs},
	}
}

// callerUser returns the user the request's credential acts for, as
// authentication read it for this request. Only a route that needs a user
// calls it, so the admin token never gets here.
func callerUser(r *http.Request) (User, error) {
	p, ok := PrincipalOf(r.Context())
	if !ok || p.User == nil {
		return User{}, errors.New("a route that needs a user was reached without one")
	}
	return *p.User, nil
}

// getOwnUser answers GET /user with the caller's User object.
func (s *Service) getOwnUser(w http.ResponseWriter, r *http.Request) error {
	u, err := callerUser(r)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, u)
	return nil
}

// updateOwnUser answers PATCH /user: it stores the body's full_name, a
// required JSON string whose empty value clears the name, as the caller's,
// with updated_at the time of the change, and answers 200 with the updated
// User object.
func (s *Service) updateOwnUser(w http.ResponseWriter, r *http.Request) error {
	caller, err := callerUser(r)
	if err != nil {
		return err
	}
	fullName, err := fullNameOf(r)
	if err != nil {
		return err
	}
	var u User
	err = s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		u, err = updateFullName(r.Context(), tx, caller.ID, fullName, s.deps.Now())
		return err
	})
	if errors.Is(err, ErrNotFound) {
		// The user was deleted after authentication read it.
		return server.NotFound("user not found")
	}
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, u)
	return nil
}

// listOwnOrgs answers GET /user/orgs: a page of the active orgs of which
// the caller is a member, by name.
func (s *Service) listOwnOrgs(w http.ResponseWriter, r *http.Request) error {
	caller, err := callerUser(r)
	if err != nil {
		return err
	}
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	after, err := pageAfter[orgPosition](page)
	if err != nil {
		return err
	}
	orgs, err := listOrgs(r.Context(), s.deps.Store.Main(), false, caller.ID, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, orgs, func(o Org) any { return orgPosition{Name: o.Name} })
}
