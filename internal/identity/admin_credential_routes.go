package identity

import (
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// adminCredentialRoutes are the admin routes of any user's API keys and
// PATs, so that an operator can see and cut off a user's credentials
// without the user. They answer what the routes of the caller's own keys
// and tokens answer, for the user the selector names.
func (s *Service) adminCredentialRoutes() []server.Route {
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/users/:id/keys", Marks: adminMark, Handler: s.forSelectedUser(s.writeKeys)},
		{Method: http.MethodGet, Pattern: "/users/:id/tokens", Marks: adminMark, Handler: s.forSelectedUser(s.writeTokens)},
		{Method: http.MethodDelete, Pattern: "/users/:id/keys/:key_id", Marks: adminMark, Handler: s.forSelectedUser(s.revokeKey)},
		{Method: http.MethodDelete, Pattern: "/users/:id/tokens/:token_id", Marks: adminMark,
			Handler: s.forSelectedUser(s.revokeToken)},
	}
}

// forSelectedUser returns a handler that resolves the path's user selector
// and answers with answer for the user it names; a selector that names no
// user is errUserNotFound.
func (s *Service) forSelectedUser(answer func(w http.ResponseWriter, r *http.Request, userID string) error) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		u, err := UserBySelector(r.Context(), s.deps.Store.Main(), server.Param(r, "id"))
		if errors.Is(err, ErrNotFound) {
			return errUserNotFound()
		}
		if err != nil {
			return err
		}
		return answer(w, r, u.ID)
	}
}
