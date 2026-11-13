package identity

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/agent-fox-dev/parley/internal/server"
)

// tokenRoutes are the routes of the caller's own personal access tokens.
func (s *Service) tokenRoutes() []server.Route {
	return []server.Route{
		// A create route whose token member is left out of a replay, so the
		// value is shown once even when the request is sent again.
		{Method: http.MethodPost, Pattern: "/user/tokens", Create: true, ShowOnce: []string{"token"},
			Scope: "tokens:manage", Marks: needsUserMark, Handler: s.createToken},
	}
}

// createToken answers POST /user/tokens: it mints a PAT for the caller's
// user from the body's name, permissions, paths and expires, and answers
// 201 with the Token object and the token's value. A token minted here is
// not bound to a workspace.
func (s *Service) createToken(w http.ResponseWriter, r *http.Request) error {
	caller, ok := PrincipalOf(r.Context())
	if !ok {
		return errors.New("a route that needs a user was reached without a caller")
	}
	req, err := mintRequestOf(r)
	if err != nil {
		return err
	}
	var token string
	var pat PAT
	err = s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		token, pat, err = MintPAT(r.Context(), tx, caller, req, s.deps.Now())
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusCreated, createdToken{tokenObject: pat.object(), Token: token})
	return nil
}

// mintRequestOf reads a token request from the body of r. It checks only
// the JSON type of each member, and leaves their values to MintPAT: name a
// string, permissions an array of strings, paths null or an array of
// strings, and expires an integer. A missing name or permissions is read as
// empty, and refused by MintPAT.
func mintRequestOf(r *http.Request) (MintRequest, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return MintRequest{}, err
	}
	var req MintRequest
	var err error
	if req.Name, _, err = stringMember(body, "name"); err != nil {
		return MintRequest{}, err
	}
	if raw, ok := body["permissions"]; ok {
		if req.Permissions, err = stringsMember(raw, "permissions"); err != nil {
			return MintRequest{}, err
		}
	}
	if raw, ok := body["paths"]; ok && string(raw) != "null" {
		if req.Paths, err = stringsMember(raw, "paths"); err != nil {
			return MintRequest{}, err
		}
	}
	if raw, ok := body["expires"]; ok {
		var days int
		if string(raw) == "null" || json.Unmarshal(raw, &days) != nil {
			return MintRequest{}, server.InvalidRequest("expires must be one of 0, 30, 60 and 90")
		}
		req.Expires = &days
	}
	return req, nil
}

// stringsMember decodes raw, the member name of a request body, as an array
// of strings; an empty array is returned as an empty, not a nil, slice.
func stringsMember(raw json.RawMessage, name string) ([]string, error) {
	var v []string
	if len(raw) == 0 || raw[0] != '[' || json.Unmarshal(raw, &v) != nil {
		return nil, server.InvalidRequest(name + " must be an array of strings")
	}
	return v, nil
}
