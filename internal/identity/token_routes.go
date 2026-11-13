package identity

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"

	"github.com/agent-fox-dev/parley/internal/server"
)

// tokenRoutes are the routes of the caller's own personal access tokens.
func (s *Service) tokenRoutes() []server.Route {
	return []server.Route{
		// A create route whose token member is left out of a replay, so the
		// value is shown once even when the request is sent again.
		{Method: http.MethodPost, Pattern: "/user/tokens", Create: true, ShowOnce: []string{"token"},
			Scope: "tokens:manage", Marks: needsUserMark, Handler: s.createToken},
		{Method: http.MethodGet, Pattern: "/user/tokens", Scope: "tokens:read", Marks: needsUserMark, Handler: s.listOwnTokens},
		{Method: http.MethodGet, Pattern: "/user/tokens/:token_id", Scope: "tokens:read", Marks: needsUserMark,
			Handler: s.getOwnToken},
		{Method: http.MethodDelete, Pattern: "/user/tokens/:token_id", Scope: "tokens:manage", Marks: needsUserMark,
			Handler: s.revokeOwnToken},
		{Method: http.MethodPut, Pattern: "/user/tokens/:token_id/permissions", Scope: "tokens:write", Marks: needsUserMark,
			Handler: s.changeOwnTokenPermissions},
		{Method: http.MethodPatch, Pattern: "/user/tokens/:token_id/permissions", Scope: "tokens:write", Marks: needsUserMark,
			Handler: s.changeOwnTokenPermissions},
		{Method: http.MethodDelete, Pattern: "/user/tokens/:token_id/permissions", Scope: "tokens:write", Marks: needsUserMark,
			Handler: s.changeOwnTokenPermissions},
	}
}

// changeOwnTokenPermissions answers PUT, PATCH and DELETE
// /user/tokens/:token_id/permissions: PUT replaces the caller's token's
// permissions with the list given, PATCH adds the scopes it does not have,
// DELETE removes the scopes given. The checks run in a fixed order, the
// owner lookup (404), the body (400), the subset rule on the scopes the
// change adds and the token's paths for a PAT caller (403), then the token's state (409), so that
// an answer tells nothing of a token the caller does not own and a change
// is judged on a valid body. A change that leaves no scope revokes the
// token in the same transaction. The answer is 200 with the token after the
// change; its paths, workspace, name and expiry never change.
func (s *Service) changeOwnTokenPermissions(w http.ResponseWriter, r *http.Request) error {
	caller, ok := PrincipalOf(r.Context())
	if !ok {
		return errors.New("a route that needs a user was reached without a caller")
	}
	now := s.deps.Now()
	var p PAT
	err := s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		cur, err := ownToken(r, tx)
		if err != nil {
			return err
		}
		given, err := permissionsChangeOf(r)
		if err != nil {
			return err
		}
		perms := changedPermissions(r.Method, cur.Permissions, given)
		// DELETE adds nothing, so the rule never refuses it.
		added := slices.DeleteFunc(slices.Clone(perms), func(scope string) bool { return slices.Contains(cur.Permissions, scope) })
		if err := CheckWidenSubset(caller, added, cur.Paths); err != nil {
			return err
		}
		if cur.RevokedAt != nil {
			return server.Conflict("token is revoked")
		}
		if expired(cur.ExpiresAt, now) {
			return server.Conflict("token is expired")
		}
		p, err = setTokenPermissions(r.Context(), tx, cur.TokenID, perms, now)
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, p)
	return nil
}

// permissionsChangeOf reads the permissions member of the body of a
// permission change, an array of strings; other members, paths and
// workspace among them, are ignored. PUT and PATCH take registered scopes
// only, and PATCH and DELETE at least one string; DELETE takes any
// string, since removing a scope a token lacks changes nothing.
func permissionsChangeOf(r *http.Request) ([]string, error) {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return nil, err
	}
	given, err := stringsMember(body["permissions"], "permissions")
	if err != nil {
		return nil, err
	}
	if len(given) == 0 && r.Method != http.MethodPut {
		return nil, server.InvalidRequest("permissions must hold at least one scope")
	}
	if r.Method != http.MethodDelete {
		for _, s := range given {
			if !IsRegisteredScope(s) {
				return nil, server.InvalidRequest(fmt.Sprintf("permissions: %q is not a registered scope", s))
			}
		}
	}
	return given, nil
}

// listOwnTokens answers GET /user/tokens: a page of the caller's tokens.
func (s *Service) listOwnTokens(w http.ResponseWriter, r *http.Request) error {
	caller, err := callerUser(r)
	if err != nil {
		return err
	}
	return s.writeTokens(w, r, caller.ID)
}

// writeTokens answers a page of the PATs of the user userID in any state,
// bound ones included, newest first: created_at descending, then token_id
// descending.
func (s *Service) writeTokens(w http.ResponseWriter, r *http.Request, userID string) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	after, err := pageAfter[tokenPosition](page)
	if err != nil {
		return err
	}
	pats, err := listTokens(r.Context(), s.deps.Store.Main(), userID, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, pats, PAT.position)
}

// ownToken returns the caller's token the path's token_id names, or
// errTokenNotFound when the caller's user does not own it.
// It reads through q, so that a route that changes the token looks it up
// in its own transaction.
func ownToken(r *http.Request, q Querier) (PAT, error) {
	caller, err := callerUser(r)
	if err != nil {
		return PAT{}, err
	}
	return tokenOfUser(r.Context(), q, caller.ID, server.Param(r, "token_id"))
}

// getOwnToken answers GET /user/tokens/:token_id with the caller's token,
// in any state.
func (s *Service) getOwnToken(w http.ResponseWriter, r *http.Request) error {
	p, err := ownToken(r, s.deps.Store.Main())
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, p)
	return nil
}

// revokeOwnToken answers DELETE /user/tokens/:token_id: it revokes the
// caller's token now, also an expired one and the one the request came
// with, and answers 200 with the token; a revoked token is answered as it
// is.
func (s *Service) revokeOwnToken(w http.ResponseWriter, r *http.Request) error {
	caller, err := callerUser(r)
	if err != nil {
		return err
	}
	return s.revokeToken(w, r, caller.ID)
}

// revokeToken revokes the PAT of the user userID the path's token_id names
// and answers 200 with it.
func (s *Service) revokeToken(w http.ResponseWriter, r *http.Request, userID string) error {
	var p PAT
	err := s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		p, err = revokeTokenOfUser(r.Context(), tx, userID, server.Param(r, "token_id"), s.deps.Now())
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, p)
	return nil
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
