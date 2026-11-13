package identity

import (
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/agent-fox-dev/parley/internal/server"
)

// orgRoutes are the routes of the orgs.
func (s *Service) orgRoutes() []server.Route {
	return []server.Route{
		{Method: http.MethodPost, Pattern: "/orgs", Create: true, Marks: adminMark, Handler: s.createOrg},
		{Method: http.MethodGet, Pattern: "/orgs", Marks: adminMark, Handler: s.listOrgs},
		// Not an admin route: a member reads an org too, see visibleOrg.
		{Method: http.MethodGet, Pattern: "/orgs/:id", Scope: "orgs:read", Handler: s.getOrg},
		{Method: http.MethodPatch, Pattern: "/orgs/:id", Marks: adminMark, Handler: s.updateOrg},
		// Not an admin route either: the members of an org read its list.
		{Method: http.MethodGet, Pattern: "/orgs/:id/members", Scope: "orgs:read", Handler: s.listMembers},
		{Method: http.MethodPut, Pattern: "/orgs/:id/members/:user_id", Marks: adminMark, Handler: s.addMember},
		{Method: http.MethodDelete, Pattern: "/orgs/:id/members/:user_id", Marks: adminMark, Handler: s.removeMember},
		{Method: http.MethodPost, Pattern: "/orgs/:id/block", Marks: adminMark, Handler: s.setOrgStatus(StatusBlocked)},
		{Method: http.MethodPost, Pattern: "/orgs/:id/unblock", Marks: adminMark, Handler: s.setOrgStatus(StatusActive)},
		{Method: http.MethodDelete, Pattern: "/orgs/:id", Marks: adminMark, Handler: s.deleteOrg},
	}
}

// setOrgStatus answers block (status blocked) and unblock (status active):
// it sets the status of the org the selector names, with updated_at the
// time of the change, and answers 200 with the Org object. The memberships
// are kept. An org that has the status already is answered as stored, and
// nothing is written, so updated_at stays.
func (s *Service) setOrgStatus(status string) server.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) error {
		ctx := r.Context()
		var o Org
		err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
			target, err := OrgBySelector(ctx, tx, server.Param(r, "id"))
			if err != nil {
				return err
			}
			if target.Status == status {
				o = target
				return nil
			}
			o, err = updateOrgStatus(ctx, tx, target.ID, status, s.deps.Now())
			return err
		})
		if errors.Is(err, ErrNotFound) {
			return errOrgNotFound()
		}
		if err != nil {
			return err
		}
		server.WriteJSON(w, http.StatusOK, o)
		return nil
	}
}

// deleteOrg answers DELETE /orgs/:id: in one store transaction it calls
// the org removers with the id of the org the selector names, then deletes
// its memberships and the org, a personal or a blocked one included, and
// answers 204. The users who were members or owner are kept. A selector
// that names no org calls no remover; a remover's error rolls the whole
// delete back and is answered 500 internal.
func (s *Service) deleteOrg(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		o, err := OrgBySelector(ctx, tx, server.Param(r, "id"))
		if errors.Is(err, ErrNotFound) {
			return errOrgNotFound()
		}
		if err != nil {
			return err
		}
		if err := s.removeOrgData(ctx, tx, o.ID); err != nil {
			return err
		}
		return deleteOrg(ctx, tx, o.ID)
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// errOrgNotFound answers a selector that names no org, and an org the
// caller may not read: the two answers are one, so that a caller learns
// nothing of an org it may not read.
func errOrgNotFound() error { return server.NotFound("org not found") }

// visibleOrg returns the org the selector of r names when the caller of r
// may read it, and errOrgNotFound when there is no such org or the caller
// may not. An admin reads every org; any other caller, a PAT of an admin
// included, reads an active org of which its user is a member.
func (s *Service) visibleOrg(r *http.Request) (Org, error) {
	ctx, db := r.Context(), s.deps.Store.Main()
	o, err := OrgBySelector(ctx, db, server.Param(r, "id"))
	if errors.Is(err, ErrNotFound) {
		return Org{}, errOrgNotFound()
	}
	if err != nil {
		return Org{}, err
	}
	p, _ := PrincipalOf(ctx)
	if isAdmin(p) {
		return o, nil
	}
	if o.Status != StatusActive || p.User == nil {
		return Org{}, errOrgNotFound()
	}
	member, err := isMember(ctx, db, o.ID, p.User.ID)
	if err != nil {
		return Org{}, err
	}
	if !member {
		return Org{}, errOrgNotFound()
	}
	return o, nil
}

// getOrg answers GET /orgs/:id with the Org object of the org the selector
// names, when the caller may read it (see visibleOrg).
func (s *Service) getOrg(w http.ResponseWriter, r *http.Request) error {
	o, err := s.visibleOrg(r)
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, o)
	return nil
}

// createOrg answers POST /orgs: it creates an active org with the body's
// name, trimmed, slug, url (default "") and owner_id (default none), and
// no membership, and answers 201 with the Org object. A name or a slug
// another org has is 409 conflict.
func (s *Service) createOrg(w http.ResponseWriter, r *http.Request) error {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	name, err := orgNameMember(body)
	if err != nil {
		return err
	}
	if name == nil {
		return server.InvalidRequest("name is required")
	}
	slug, present, err := stringMember(body, "slug")
	if err != nil {
		return err
	}
	if !present {
		return server.InvalidRequest("slug is required")
	}
	if !validSlug(slug) {
		return server.InvalidRequest("slug must be 1 to 128 characters of a-z, 0-9, _ and -, starting and ending with a letter or digit")
	}
	url, _, err := stringMember(body, "url")
	if err != nil {
		return err
	}
	ownerID, hasOwner, err := stringMember(body, "owner_id")
	if err != nil {
		return err
	}
	ctx := r.Context()
	var o Org
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		if hasOwner {
			_, err := UserByID(ctx, tx, ownerID)
			if errors.Is(err, ErrNotFound) {
				return server.InvalidRequest("owner_id is not the id of a user")
			}
			if err != nil {
				return err
			}
		}
		var err error
		o, err = createOrg(ctx, tx, Org{Name: *name, Slug: slug, URL: url, OwnerID: ownerID, Status: StatusActive,
			CreatedAt: s.deps.Now()})
		return err
	})
	if err != nil {
		return asConflict(err)
	}
	server.WriteJSON(w, http.StatusCreated, o)
	return nil
}

// listOrgs answers GET /orgs: a page of the orgs by name, the active ones
// only unless include_blocked is exactly "true".
func (s *Service) listOrgs(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	after, err := pageAfter[orgPosition](page)
	if err != nil {
		return err
	}
	includeBlocked := r.URL.Query().Get("include_blocked") == "true"
	orgs, err := listOrgs(r.Context(), s.deps.Store.Main(), includeBlocked, "", after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, orgs, func(o Org) any { return orgPosition{Name: o.Name} })
}

// updateOrg answers PATCH /orgs/:id: it stores the body's name, trimmed,
// and url, each when sent, at least one of them, as those of the org the
// selector names, with updated_at the time of the change, and answers 200
// with the updated Org object. A slug member is ignored like any unknown
// one. The body is checked before the org is looked up, and a name another
// org has is 409 conflict.
func (s *Service) updateOrg(w http.ResponseWriter, r *http.Request) error {
	var body map[string]json.RawMessage
	if err := server.DecodeJSON(r, &body); err != nil {
		return err
	}
	name, err := orgNameMember(body)
	if err != nil {
		return err
	}
	url, hasURL, err := stringMember(body, "url")
	if err != nil {
		return err
	}
	if name == nil && !hasURL {
		return server.InvalidRequest("name or url is required")
	}
	var newURL *string
	if hasURL {
		newURL = &url
	}
	ctx := r.Context()
	var o Org
	err = s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		target, err := OrgBySelector(ctx, tx, server.Param(r, "id"))
		if err != nil {
			return err
		}
		o, err = updateOrg(ctx, tx, target.ID, name, newURL, s.deps.Now())
		return err
	})
	if errors.Is(err, ErrNotFound) {
		return errOrgNotFound()
	}
	if err != nil {
		return asConflict(err)
	}
	server.WriteJSON(w, http.StatusOK, o)
	return nil
}

// orgNameMember returns the member name of a decoded JSON object trimmed of
// leading and trailing white space, nil when it is absent. A name that is
// not a JSON string, or is empty once trimmed, is 400 invalid_request.
func orgNameMember(body map[string]json.RawMessage) (*string, error) {
	name, present, err := stringMember(body, "name")
	if err != nil || !present {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		return nil, server.InvalidRequest("name must not be empty")
	}
	return &name, nil
}

// pageAfter returns the position page continues after, decoded as a T, or
// nil for a first page.
func pageAfter[T any](page server.Page) (*T, error) {
	var pos T
	hasAfter, err := page.After(&pos)
	if err != nil || !hasAfter {
		return nil, err
	}
	return &pos, nil
}

// listMembers answers GET /orgs/:id/members: a page of the memberships of
// the org the selector names, by username, when the caller may read the
// org (see visibleOrg), so a blocked org's list is an admin's alone. The
// page is read before the org is looked up: a bad limit or cursor is 400
// for every caller and tells nothing of the org.
func (s *Service) listMembers(w http.ResponseWriter, r *http.Request) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	after, err := pageAfter[memberPosition](page)
	if err != nil {
		return err
	}
	o, err := s.visibleOrg(r)
	if err != nil {
		return err
	}
	items, err := listMembers(r.Context(), s.deps.Store.Main(), o.ID, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, items, func(m memberItem) any { return memberPosition{Username: m.Username} })
}

// addMember answers PUT /orgs/:id/members/:user_id: it makes the user the
// user selector names a member of the org the org selector names, from the
// time of the request, and answers 204. An existing membership is left as
// it is, and blocked users and blocked orgs are accepted.
func (s *Service) addMember(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		o, u, err := orgAndUser(r, tx)
		if err != nil {
			return err
		}
		return addMembership(ctx, tx, o.ID, u.ID, s.deps.Now())
	})
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// removeMember answers DELETE /orgs/:id/members/:user_id: it deletes the
// membership of the user the user selector names in the org the org
// selector names, keeping the user, the org and its owner_id, and answers
// 204. A user who is no member is 404 not_found.
func (s *Service) removeMember(w http.ResponseWriter, r *http.Request) error {
	ctx := r.Context()
	err := s.deps.Store.Tx(ctx, func(tx *sql.Tx) error {
		o, u, err := orgAndUser(r, tx)
		if err != nil {
			return err
		}
		return removeMembership(ctx, tx, o.ID, u.ID)
	})
	if errors.Is(err, ErrNotFound) {
		return server.NotFound("membership not found")
	}
	if err != nil {
		return err
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// orgAndUser reads, in tx, the org the selector :id of r names and the user
// the selector :user_id names, the org first. No org is errOrgNotFound and
// no user errUserNotFound.
func orgAndUser(r *http.Request, tx *sql.Tx) (Org, User, error) {
	o, err := OrgBySelector(r.Context(), tx, server.Param(r, "id"))
	if errors.Is(err, ErrNotFound) {
		return Org{}, User{}, errOrgNotFound()
	}
	if err != nil {
		return Org{}, User{}, err
	}
	u, err := UserBySelector(r.Context(), tx, server.Param(r, "user_id"))
	if errors.Is(err, ErrNotFound) {
		return Org{}, User{}, errUserNotFound()
	}
	if err != nil {
		return Org{}, User{}, err
	}
	return o, u, nil
}
