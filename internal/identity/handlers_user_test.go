package identity_test

import (
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"regexp"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// needsUserRoute is a route that needs a user, with a body it accepts, nil
// for a route that takes none.
type needsUserRoute struct {
	method, path string
	body         any
}

// needsUserRoutes are every route of the identity service that needs a
// user.
var needsUserRoutes = []needsUserRoute{
	{http.MethodGet, "/api/v1/user", nil},
	{http.MethodPatch, "/api/v1/user", map[string]any{"full_name": "x"}},
	{http.MethodGet, "/api/v1/user/orgs", nil},
}

// wholeSecondsUTC is an RFC 3339 time in UTC with whole seconds.
var wholeSecondsUTC = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\dZ$`)

// withoutMembers returns obj without the members names.
func withoutMembers(obj map[string]any, names ...string) map[string]any {
	out := maps.Clone(obj)
	for _, n := range names {
		delete(out, n)
	}
	return out
}

func TestOwnProfile_AnswersTheCallersUserObject(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for name, caller := range map[string]string{"API key": h.ku, "PAT": h.pu} {
		t.Run(name, func(t *testing.T) {
			r := h.get("/api/v1/user", caller)
			if r.status != http.StatusOK {
				t.Fatalf("GET /user = %d %s, want 200", r.status, r.body)
			}
			if got := slices.Sorted(maps.Keys(r.json)); !slices.Equal(got, userMembers) {
				t.Errorf("members = %v, want %v", got, userMembers)
			}
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
	// In order: the empty string clears the name the first case set.
	for _, fullName := range []string{"Alice A.", ""} {
		t.Run(fmt.Sprintf("%q", fullName), func(t *testing.T) {
			before := h.get("/api/v1/user", h.ku).json
			h.advance(time.Hour)
			r := h.call(http.MethodPatch, "/api/v1/user", h.ku, map[string]any{"full_name": fullName})
			if r.status != http.StatusOK {
				t.Fatalf("PATCH /user = %d %s, want 200", r.status, r.body)
			}
			if r.json["full_name"] != fullName {
				t.Errorf("full_name = %v, want %q", r.json["full_name"], fullName)
			}
			if want := h.now().Format(time.RFC3339); r.json["updated_at"] != want {
				t.Errorf("updated_at = %v, want the time of the change %s", r.json["updated_at"], want)
			}
			if got, want := withoutMembers(r.json, "full_name", "updated_at"), withoutMembers(before, "full_name", "updated_at"); !reflect.DeepEqual(got, want) {
				t.Errorf("other members = %v, want them unchanged: %v", got, want)
			}
			if after := h.get("/api/v1/user", h.ku).json; !reflect.DeepEqual(after, r.json) {
				t.Errorf("GET /user after the change = %v, want %v", after, r.json)
			}
		})
	}
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
			before, stored := h.get("/api/v1/user", h.ku).json, h.snapshot()
			r := h.call(http.MethodPatch, "/api/v1/user", h.ku, c.body)
			checkRefusal(t, "PATCH /user", r, http.StatusBadRequest, "invalid_request", c.message)
			if after := h.get("/api/v1/user", h.ku).json; !reflect.DeepEqual(after, before) {
				t.Errorf("GET /user after the refusal = %v, want %v", after, before)
			}
			if h.snapshot() != stored {
				t.Errorf("the store changed")
			}
		})
	}
}

func TestNeedsUserRoutes_RefuseTheAdminToken(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for _, rt := range needsUserRoutes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			before := h.snapshot()
			r := h.call(rt.method, rt.path, h.admin, rt.body)
			checkRefusal(t, "admin token", r, http.StatusForbidden, "forbidden", "this route needs a user")
			if h.snapshot() != before {
				t.Errorf("the store changed")
			}
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
	r := h.get("/api/v1/user/orgs", h.ku)
	if r.status != http.StatusOK {
		t.Fatalf("GET /user/orgs = %d %s, want 200", r.status, r.body)
	}
	if got, want := itemNames(t, r), []string{"alice", "beta", "zeta"}; !slices.Equal(got, want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	if got := slices.Sorted(maps.Keys(r.json["items"].([]any)[0].(map[string]any))); !slices.Equal(got, orgMembers) {
		t.Errorf("members = %v, want %v", got, orgMembers)
	}
	ids := itemIDs(t, r)
	if got, requests := h.walk("/api/v1/user/orgs", h.ku, 2); !slices.Equal(got, ids) || requests != 2 {
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
	// The production scope check, which is not part of this service,
	// compares with the implication rules; this one compares the declared
	// scope with the PAT's own.
	h.srv.SetScopeCheck(func(r *http.Request) error {
		p, _ := identity.PrincipalOf(r.Context())
		m, _ := server.MatchedRoute(r)
		if p.Type == identity.TypePAT && m.Route.Scope != "" && !slices.Contains(p.Permissions, m.Route.Scope) {
			return server.InsufficientScope("the token does not hold the scope " + m.Route.Scope)
		}
		return nil
	})
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
			others := slices.DeleteFunc(slices.Clone(everyScope), func(s string) bool { return s == c.scope })
			without, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: others})
			only, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: []string{c.scope}})
			before := h.snapshot()
			r := h.call(c.method, c.path, "Bearer "+without, c.body)
			if r.status != http.StatusForbidden || r.errorField("error_type") != "insufficient_scope" {
				t.Errorf("without %s: answer = %d %s, want 403 insufficient_scope", c.scope, r.status, r.body)
			}
			if h.snapshot() != before {
				t.Errorf("without %s: the store changed", c.scope)
			}
			if r := h.call(c.method, c.path, "Bearer "+only, c.body); r.status != http.StatusOK {
				t.Errorf("with %s alone: answer = %d %s, want 200", c.scope, r.status, r.body)
			}
		})
	}
}
