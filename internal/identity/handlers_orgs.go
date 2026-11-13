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
	}
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
	if ce := (conflictError{}); errors.As(err, &ce) {
		return server.Conflict(ce.msg)
	}
	if err != nil {
		return err
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
	var pos orgPosition
	hasAfter, err := page.After(&pos)
	if err != nil {
		return err
	}
	after := &pos
	if !hasAfter {
		after = nil
	}
	includeBlocked := r.URL.Query().Get("include_blocked") == "true"
	orgs, err := listOrgs(r.Context(), s.deps.Store.Main(), includeBlocked, after, page.Limit+1)
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
	if ce := (conflictError{}); errors.As(err, &ce) {
		return server.Conflict(ce.msg)
	}
	if err != nil {
		return err
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
