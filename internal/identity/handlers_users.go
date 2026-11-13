package identity

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// adminMark declares a route as an admin route, for AdminCheck. Admin routes
// declare no scope: no PAT passes the admin check.
var adminMark = map[string]string{MarkAdmin: "true"}

// userRoutes are the routes of the users.
func (s *Service) userRoutes() []server.Route {
	return []server.Route{
		{Method: http.MethodPost, Pattern: "/users", Create: true, Marks: adminMark, Handler: s.createUser},
	}
}

// createUser answers POST /users: it creates an active user with the role
// user, the user's personal org and the membership, in one transaction, and
// answers 201 with the User object. The role is user also when the e-mail
// is the bootstrap admin e-mail: only a first login promotes.
func (s *Service) createUser(w http.ResponseWriter, r *http.Request) error {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	nu := NewUser{CreatedAt: s.deps.Now()}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"username", &nu.Username}, {"email", &nu.Email}, {"provider", &nu.Provider}, {"provider_id", &nu.ProviderID}} {
		v, present, err := stringMember(body, f.name)
		if err != nil {
			return err
		}
		if !present || v == "" {
			return server.InvalidRequest(f.name + " is required")
		}
		*f.dst = v
	}
	fullName, _, err := stringMember(body, "full_name")
	if err != nil {
		return err
	}
	nu.FullName = fullName
	var u User
	err = s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		u, _, err = CreateUserAndPersonalOrg(r.Context(), tx, nu, RoleUser)
		return err
	})
	if ce := (conflictError{}); errors.As(err, &ce) {
		return server.Conflict(ce.msg)
	}
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, u)
	return nil
}

// stringMember returns the member name of a decoded JSON object and whether
// it is present. A member that is present and not a JSON string, null
// included, is 400 invalid_request.
func stringMember(body map[string]json.RawMessage, name string) (string, bool, error) {
	raw, ok := body[name]
	if !ok {
		return "", false, nil
	}
	var v string
	if len(raw) == 0 || raw[0] != '"' || json.Unmarshal(raw, &v) != nil {
		return "", true, server.InvalidRequest(name + " must be a string")
	}
	return v, true, nil
}
