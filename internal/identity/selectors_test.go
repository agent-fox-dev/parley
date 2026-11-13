package identity_test

import (
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"uuid"
)

// userSelectorRoutes are the admin routes that name a user by a selector,
// ":user" in their path, each with a body it accepts (nil for none). The
// membership routes name the org acme. The user delete comes last, so that
// the routes before it find the user as the team hub created her.
var userSelectorRoutes = []testRoute{
	{http.MethodGet, "/api/v1/users/:user", nil},
	{http.MethodPatch, "/api/v1/users/:user", map[string]any{"full_name": "x"}},
	{http.MethodPost, "/api/v1/users/:user/promote", nil},
	{http.MethodPost, "/api/v1/users/:user/demote", nil},
	{http.MethodPost, "/api/v1/users/:user/block", nil},
	{http.MethodPost, "/api/v1/users/:user/unblock", nil},
	{http.MethodPut, "/api/v1/orgs/acme/members/:user", nil},
	{http.MethodDelete, "/api/v1/orgs/acme/members/:user", nil},
	{http.MethodDelete, "/api/v1/users/:user", nil},
}

// orgSelectorRoutes are the admin routes that name an org by a selector,
// ":org" in their path, each with a body it accepts (nil for none). The
// membership routes name alice. The org delete comes last.
var orgSelectorRoutes = []testRoute{
	{http.MethodGet, "/api/v1/orgs/:org", nil},
	{http.MethodPatch, "/api/v1/orgs/:org", map[string]any{"url": "u"}},
	{http.MethodGet, "/api/v1/orgs/:org/members", nil},
	{http.MethodDelete, "/api/v1/orgs/:org/members/alice", nil},
	{http.MethodPut, "/api/v1/orgs/:org/members/alice", nil},
	{http.MethodPost, "/api/v1/orgs/:org/block", nil},
	{http.MethodPost, "/api/v1/orgs/:org/unblock", nil},
	{http.MethodDelete, "/api/v1/orgs/:org", nil},
}

// deletesAlice sends the user delete at path as the admin token and reports
// unless the answer is 204 without a body and alice is gone: her going
// shows that the selector reached her. It then stores alice again, the
// record the team hub created, so that the next request finds her.
func (h teamHub) deletesAlice(t *testing.T, path string) {
	t.Helper()
	if r := h.call(http.MethodDelete, path, h.admin, nil); r.status != http.StatusNoContent || len(r.body) != 0 {
		t.Errorf("answer = %d %s, want 204 without a body", r.status, r.body)
	}
	if n := h.count(`SELECT count(*) FROM users WHERE id = ?`, h.alice.ID); n != 0 {
		t.Errorf("alice is still there")
	}
	a := h.alice
	h.exec(`INSERT OR IGNORE INTO users (id, username, email, full_name, role, status, provider, provider_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, a.ID, a.Username, a.Email, a.FullName, a.Role, a.Status, a.Provider, a.ProviderID,
		a.CreatedAt.UnixNano(), a.UpdatedAt.UnixNano())
}

// deletesAcme sends the org delete at path as the admin token and reports
// unless the answer is 204 without a body and the org acme, whose id is
// acme, is gone: its going shows that the selector reached it. It then
// stores acme again, with the same id and root as its member, so that the
// next request finds it.
func (h teamHub) deletesAcme(t *testing.T, acme, path string) {
	t.Helper()
	if r := h.call(http.MethodDelete, path, h.admin, nil); r.status != http.StatusNoContent || len(r.body) != 0 {
		t.Errorf("answer = %d %s, want 204 without a body", r.status, r.body)
	}
	if n := h.count(`SELECT count(*) FROM orgs WHERE id = ?`, acme); n != 0 {
		t.Errorf("acme is still there")
	}
	now := h.now().UnixNano()
	h.exec(`INSERT OR IGNORE INTO orgs (id, name, slug, status, created_at, updated_at) VALUES (?, 'Acme Inc', 'acme', 'active', ?, ?)`,
		acme, now, now)
	h.setMember(acme, h.root.ID, true)
}

// reachesAliceInAcme sends the membership route rt, at path, as the admin
// token, with alice a member of the org acme before a DELETE and not one
// before a PUT. It reports unless the answer is 204 without a body and
// alice is a member of acme after a PUT and not after a DELETE: the change
// of that one membership shows that the route's selectors reached alice and
// acme.
func (h teamHub) reachesAliceInAcme(t *testing.T, rt testRoute, acme, path string) {
	t.Helper()
	h.setMember(acme, h.alice.ID, rt.method == http.MethodDelete)
	if r := h.call(rt.method, path, h.admin, rt.body); r.status != http.StatusNoContent || len(r.body) != 0 {
		t.Errorf("answer = %d %s, want 204 without a body", r.status, r.body)
	}
	isMember := h.count(`SELECT count(*) FROM memberships WHERE org_id = ? AND user_id = ?`, acme, h.alice.ID) == 1
	if want := rt.method == http.MethodPut; isMember != want {
		t.Errorf("alice is a member of acme: %t, want %t", isMember, want)
	}
}

// setMember makes the user userID a member of the org orgID from now when
// member is true, and no member when it is false.
func (h *testHub) setMember(orgID, userID string, member bool) {
	h.t.Helper()
	h.exec(`DELETE FROM memberships WHERE org_id = ? AND user_id = ?`, orgID, userID)
	if member {
		h.addMember(orgID, userID)
	}
}

// answeredIDs returns the ids of the records an answer is about: the id of
// a User or Org object, or the org_id of each item of a member list.
func answeredIDs(t *testing.T, r response) []string {
	t.Helper()
	if _, ok := r.json["items"]; ok {
		return itemValues(t, r, "org_id")
	}
	id, _ := r.json["id"].(string)
	return []string{id}
}

func TestSelectors_ReachTheSameRecordByEachOfItsSelectors(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	acme := h.seedOrg("Acme Inc", "acme")
	h.addMember(acme, h.root.ID)
	for _, c := range []struct {
		placeholder, id string
		routes          []testRoute
		selectors       []string
		deletes         func(t *testing.T, path string)
	}{
		{":user", h.alice.ID, userSelectorRoutes, []string{h.alice.ID, "alice@example.com", "alice"}, h.deletesAlice},
		{":org", acme, orgSelectorRoutes, []string{acme, "acme"},
			func(t *testing.T, path string) { h.deletesAcme(t, acme, path) }},
	} {
		for _, rt := range c.routes {
			for _, sel := range c.selectors {
				path := strings.ReplaceAll(rt.path, c.placeholder, sel)
				t.Run(rt.method+" "+path, func(t *testing.T) {
					switch {
					case strings.Contains(rt.path, "/members/"):
						h.reachesAliceInAcme(t, rt, acme, path)
					case rt.method == http.MethodDelete:
						c.deletes(t, path)
					default:
						r := h.call(rt.method, path, h.admin, rt.body)
						ids := answeredIDs(t, r)
						if r.status != http.StatusOK || len(ids) == 0 || slices.ContainsFunc(ids, func(id string) bool { return id != c.id }) {
							t.Errorf("answer = %d %s, want 200 about %s alone", r.status, r.body, c.id)
						}
					}
				})
			}
		}
	}
}

func TestSelectors_AnswerNotFoundForNoRecordAndChangeNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.addMember(h.seedOrg("Acme Inc", "acme"), h.alice.ID)
	for _, c := range []struct {
		placeholder, message string
		routes               []testRoute
		selectors            map[string]string
	}{
		{":user", "user not found", userSelectorRoutes, map[string]string{
			"a random UUID":            uuid.NewV4().String(),
			"an unknown e-mail":        "nobody@example.com",
			"an unknown username":      "nobody",
			"a username in other case": "ALICE",
			"an e-mail in upper case":  "ALICE@EXAMPLE.COM",
		}},
		{":org", "org not found", orgSelectorRoutes, map[string]string{
			"a random UUID":          uuid.NewV4().String(),
			"an unknown slug":        "nope",
			"the slug in upper case": "ACME",
			"the name":               url.PathEscape("Acme Inc"),
		}},
	} {
		for _, rt := range c.routes {
			for name, sel := range c.selectors {
				t.Run(rt.method+" "+rt.path+"/"+name, func(t *testing.T) {
					r := h.callUnchanged(t, rt.method, strings.ReplaceAll(rt.path, c.placeholder, sel), h.admin, rt.body)
					checkRefusal(t, "answer", r, http.StatusNotFound, "not_found", c.message)
				})
			}
		}
	}
}
