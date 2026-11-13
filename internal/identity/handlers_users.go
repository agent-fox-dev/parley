package identity

import (
	"context"
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
		{Method: http.MethodGet, Pattern: "/users", Marks: adminMark, Handler: s.listUsers},
		{Method: http.MethodGet, Pattern: "/users/:id", Marks: adminMark, Handler: s.getUser},
		{Method: http.MethodPatch, Pattern: "/users/:id", Marks: adminMark, Handler: s.updateUser},
		{Method: http.MethodPost, Pattern: "/users/:id/promote", Marks: adminMark, Handler: s.setRole(RoleAdmin)},
		{Method: http.MethodPost, Pattern: "/users/:id/demote", Marks: adminMark, Handler: s.setRole(RoleUser)},
		{Method: http.MethodPost, Pattern: "/users/:id/block", Marks: adminMark, Handler: s.setStatus(StatusBlocked)},
		{Method: http.MethodPost, Pattern: "/users/:id/unblock", Marks: adminMark, Handler: s.setStatus(StatusActive)},
		{Method: http.MethodDelete, Pattern: "/users/:id", Marks: adminMark, Handler: s.deleteUser},
	}
}

// errUserNotFound answers a selector that names no user.
func errUserNotFound() error { return server.NotFound("user not found") }

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

// listUsers answers GET /users: a page of the users, newest first, the
// active ones only unless include_blocked is exactly "true".
func (s *Service) listUsers(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	var pos userPosition
	hasAfter, err := page.After(&pos)
	if err != nil {
		return err
	}
	after := &pos
	if !hasAfter {
		after = nil
	}
	includeBlocked := r.URL.Query().Get("include_blocked") == "true"
	users, err := listUsers(r.Context(), s.deps.Store.Main(), includeBlocked, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, users, func(u User) any {
		return userPosition{CreatedAt: u.CreatedAt.UnixNano(), ID: u.ID}
	})
}

// getUser answers GET /users/:id with the User object of the user the
// selector names, active or blocked.
func (s *Service) getUser(w http.ResponseWriter, r *http.Request) error {
	u, err := UserBySelector(r.Context(), s.deps.Store.Main(), server.Param(r, "id"))
	if errors.Is(err, ErrNotFound) {
		return errUserNotFound()
	}
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, u)
	return nil
}

// updateUser answers PATCH /users/:id: it stores the body's full_name, by
// the rule of PATCH /user, as the full name of the user the selector names,
// with updated_at the time of the change, and answers 200 with the updated
// User object. The body is checked before the user is looked up.
func (s *Service) updateUser(w http.ResponseWriter, r *http.Request) error {
	fullName, err := fullNameOf(r)
	if err != nil {
		return err
	}
	var u User
	err = s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		target, err := UserBySelector(r.Context(), tx, server.Param(r, "id"))
		if err != nil {
			return err
		}
		u, err = updateFullName(r.Context(), tx, target.ID, fullName, s.deps.Now())
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return errUserNotFound()
	}
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, u)
	return nil
}

// fullNameOf reads a profile update's body: a JSON object whose member
// full_name is required and a JSON string, the empty string included.
func fullNameOf(r *http.Request) (string, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return "", err
	}
	fullName, present, err := stringMember(body, "full_name")
	if err != nil {
		return "", err
	}
	if !present {
		return "", server.InvalidRequest("full_name is required")
	}
	return fullName, nil
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

// setRole answers promote (role admin) and demote (role user): it gives the
// user the selector names the role, with updated_at the time of the change,
// and answers 200 with the User object. A user who has the role already is
// answered as stored, and nothing is written. A demotion of the only active
// admin is 409 conflict.
func (s *Service) setRole(role string) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		return s.changeUser(w, r, func(ctx context.Context, tx *sql.Tx, u User) (User, error) {
			if u.Role == role {
				return u, nil
			}
			if err := guardLastAdmin(ctx, tx, u); err != nil {
				return User{}, err
			}
			return updateRole(ctx, tx, u.ID, role, s.deps.Now())
		})
	}
}

// setStatus answers block (status blocked) and unblock (status active): it
// gives the user the selector names the status, with updated_at the time of
// the change, and answers 200 with the User object. A user who has the
// status already is answered as stored, and nothing is written. Blocking
// has no last-admin guard, because the admin token can always unblock.
func (s *Service) setStatus(status string) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		return s.changeUser(w, r, func(ctx context.Context, tx *sql.Tx, u User) (User, error) {
			if u.Status == status {
				return u, nil
			}
			return updateStatus(ctx, tx, u.ID, status, s.deps.Now())
		})
	}
}

// changeUser reads the user the selector names and applies change to it in
// one write transaction, and answers 200 with the user change returns. No
// user is 404 not_found, and a conflict of change is 409 conflict.
func (s *Service) changeUser(w http.ResponseWriter, r *http.Request,
	change func(ctx context.Context, tx *sql.Tx, u User) (User, error)) error {
	ctx := r.Context()
	var u User
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		target, err := UserBySelector(ctx, tx, server.Param(r, "id"))
		if err != nil {
			return err
		}
		u, err = change(ctx, tx, target)
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return errUserNotFound()
	}
	if ce := (conflictError{}); errors.As(err, &ce) {
		return server.Conflict(ce.msg)
	}
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, u)
	return nil
}

// deleteUser answers DELETE /users/:id: in one store transaction it reads
// the user the selector names, applies the last-admin guard, calls the user
// removers with the user's id, then deletes the user with the user's API
// keys, PATs and memberships and clears owner_id on the orgs the user
// owned, and answers 204. The orgs are kept, so no org remover is called.
// No user is 404 not_found and the only active admin 409 conflict, both
// before any remover; a remover's error rolls the whole delete back and is
// answered 500 internal.
func (s *Service) deleteUser(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		u, err := UserBySelector(ctx, tx, server.Param(r, "id"))
		if err != nil {
			return err
		}
		if err := guardLastAdmin(ctx, tx, u); err != nil {
			return err
		}
		if err := s.removeUserData(ctx, tx, u.ID); err != nil {
			return err
		}
		return deleteUser(ctx, tx, u.ID, s.deps.Now())
	})
	if errors.Is(err, ErrNotFound) {
		return errUserNotFound()
	}
	if ce := (conflictError{}); errors.As(err, &ce) {
		return server.Conflict(ce.msg)
	}
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}
