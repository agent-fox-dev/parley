package identity_test

import (
	"fmt"
	"net/http"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// needsUserRoutes are every route of the identity service that needs a
// user.
var needsUserRoutes = []testRoute{
	{http.MethodGet, "/api/v1/user", nil},
	{http.MethodPatch, "/api/v1/user", map[string]any{"full_name": "x"}},
	{http.MethodGet, "/api/v1/user/orgs", nil},
	{http.MethodGet, "/api/v1/user/keys", nil},
	{http.MethodPost, "/api/v1/user/keys/x/refresh", nil},
	{http.MethodDelete, "/api/v1/user/keys/x", nil},
	{http.MethodPost, "/api/v1/user/tokens", tokenBody(nil)},
	{http.MethodGet, "/api/v1/user/tokens", nil},
	{http.MethodGet, "/api/v1/user/tokens/x", nil},
	{http.MethodDelete, "/api/v1/user/tokens/x", nil},
	{http.MethodPut, "/api/v1/user/tokens/x/permissions", map[string]any{"permissions": []string{"git:read"}}},
	{http.MethodPatch, "/api/v1/user/tokens/x/permissions", map[string]any{"permissions": []string{"git:read"}}},
	{http.MethodDelete, "/api/v1/user/tokens/x/permissions", map[string]any{"permissions": []string{"git:read"}}},
}

// wholeSecondsUTC is an RFC 3339 time in UTC with whole seconds.
var wholeSecondsUTC = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)

func TestOwnProfile_AnswersTheCallersUserObject(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for name, caller := range map[string]string{"API key": h.ku, "PAT": h.pu} {
		t.Run(name, func(t *testing.T) {
			r := wantStatus(t, "GET /user", h.get("/api/v1/user", caller), http.StatusOK)
			checkMembers(t, r.json, userMembers)
			for member, want := range map[string]any{"id": h.alice.ID, "username": "alice", "role": "user", "status": "active"} {
				if r.json[member] != want {
					t.Errorf("%s = %v, want %v", member, r.json[member], want)
				}
			}
			for _, member := range []string{"created_at", "updated_at"} {
				if s, _ := r.json[member].(string); !wholeSecondsUTC.MatchString(s) {
					t.Errorf("%s = %v, want RFC 3339 UTC with whole seconds", member, r.json[member])
				}
			}
		})
	}
}

func TestUpdateOwnProfile_StoresTheFullNameAndTheTimeOfTheChange(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	own := func(fullName string) recordChange {
		return recordChange{name: fmt.Sprintf("%q", fullName), method: http.MethodPatch, path: "/api/v1/user", caller: h.ku,
			get: "/api/v1/user", body: map[string]any{"full_name": fullName}, changed: map[string]any{"full_name": fullName}}
	}
	// In order: the empty string clears the name the first change set.
	h.checkChanges(t, []recordChange{own("Alice A."), own("")})
}

func TestUpdateOwnProfile_RejectsABodyWithoutAStringFullName(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.advance(time.Hour)
	for _, c := range []struct {
		name    string
		body    any
		message string
	}{
		{"empty object", map[string]any{}, "full_name is required"},
		{"another member", map[string]any{"name": "x"}, "full_name is required"},
		{"a number", map[string]any{"full_name": 5}, "full_name must be a string"},
		{"null", map[string]any{"full_name": nil}, "full_name must be a string"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := h.callUnchanged(t, http.MethodPatch, "/api/v1/user", h.ku, c.body)
			checkRefusal(t, "PATCH /user", r, http.StatusBadRequest, "invalid_request", c.message)
		})
	}
}

func TestNeedsUserRoutes_RefuseTheAdminToken(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for _, rt := range needsUserRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			r := h.callUnchanged(t, rt.method, rt.path, h.admin, rt.body)
			checkRefusal(t, "admin token", r, http.StatusForbidden, "forbidden", "this route needs a user")
		})
	}
}

func TestOwnOrgs_ListsTheActiveOrgsOfTheCallerByName(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for _, name := range []string{"zeta", "beta"} {
		h.addMember(h.seedOrg(name, name), h.alice.ID)
	}
	blocked := h.seedOrg("blocked-co", "blocked-co")
	h.setOrgStatus(blocked, identity.StatusBlocked)
	h.addMember(blocked, h.alice.ID)
	h.seedOrg("other", "other")
	r := wantStatus(t, "GET /user/orgs", h.get("/api/v1/user/orgs", h.ku), http.StatusOK)
	if got, want := itemValues(t, r, "name"), []string{"alice", "beta", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	checkMembers(t, r.json["items"].([]any)[0].(map[string]any), orgMembers)
	ids := itemValues(t, r, "id")
	if got, requests := h.walkBy("/api/v1/user/orgs", h.ku, 2, "id"); !slices.Equal(got, ids) || requests != 2 {
		t.Errorf("pages of 2 = %v in %d requests, want %v in 2", got, requests, ids)
	}
	h.exec(`DELETE FROM memberships WHERE user_id = ?`, h.alice.ID)
	r = h.get("/api/v1/user/orgs", h.ku)
	items, ok := r.json["items"].([]any)
	if next, present := r.json["next_cursor"]; r.status != http.StatusOK || !ok || len(items) != 0 || !present || next != nil {
		t.Errorf("GET /user/orgs without memberships = %d %s, want 200 with items [] and next_cursor null", r.status, r.body)
	}
}

func TestScopes_RefuseAPATWithoutTheDeclaredScopeBeforeAnythingElse(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for _, c := range []struct {
		method, path, scope string
		body                any
	}{
		{http.MethodGet, "/api/v1/user", "users:read", nil},
		{http.MethodPatch, "/api/v1/user", "users:write", map[string]any{"full_name": "Changed"}},
		{http.MethodGet, "/api/v1/user/orgs", "orgs:read", nil},
		{http.MethodGet, "/api/v1/orgs/alice", "orgs:read", nil},
		{http.MethodGet, "/api/v1/orgs/alice/members", "orgs:read", nil},
	} {
		t.Run(c.method+" "+c.path, func(t *testing.T) {
			others := slices.DeleteFunc(slices.Clone(everyScope), func(s string) bool { return identity.Satisfies([]string{s}, c.scope) })
			without, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: others})
			only, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{c.scope}})
			r := h.callUnchanged(t, c.method, c.path, "Bearer "+without, c.body)
			checkRefusal(t, "without "+c.scope, r, http.StatusForbidden, "insufficient_scope", "this route needs the scope "+c.scope)
			if r := h.call(c.method, c.path, "Bearer "+only, c.body); r.status != http.StatusOK {
				t.Errorf("with %s alone: answer = %d %s, want 200", c.scope, r.status, r.body)
			}
		})
	}
}
