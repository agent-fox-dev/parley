package identity_test

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// orgMembers are the members of the Org object.
var orgMembers = []string{"created_at", "id", "name", "owner_id", "slug", "status", "updated_at", "url"}

// memberItemMembers are the members of an item of an org's member list.
var memberItemMembers = []string{"created_at", "email", "org_id", "user_id", "username"}

// orgSelectorRoutes are the admin routes that name an org by a selector,
// ":org" in their path, each with a body it accepts (nil for none). The
// membership routes name alice.
var orgSelectorRoutes = []adminRoute{
	{http.MethodGet, "/api/v1/orgs/:org", nil},
	{http.MethodPatch, "/api/v1/orgs/:org", map[string]any{"url": "u"}},
	{http.MethodGet, "/api/v1/orgs/:org/members", nil},
	{http.MethodDelete, "/api/v1/orgs/:org/members/alice", nil},
	{http.MethodPut, "/api/v1/orgs/:org/members/alice", nil},
	{http.MethodPost, "/api/v1/orgs/:org/block", nil},
	{http.MethodPost, "/api/v1/orgs/:org/unblock", nil},
	{http.MethodDelete, "/api/v1/orgs/:org", nil},
}

// membershipRoute reports whether rt adds or removes a membership.
func membershipRoute(rt adminRoute) bool { return strings.Contains(rt.path, "/members/") }

// orgDeleteRoute reports whether rt deletes the org.
func orgDeleteRoute(rt adminRoute) bool {
	return rt.method == http.MethodDelete && !membershipRoute(rt)
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
func (h teamHub) reachesAliceInAcme(t *testing.T, rt adminRoute, acme, path string) {
	t.Helper()
	h.setMember(acme, h.alice.ID, rt.method == http.MethodDelete)
	if r := h.call(rt.method, path, h.admin, rt.body); r.status != http.StatusNoContent || len(r.body) != 0 {
		t.Errorf("answer = %d %s, want 204 without a body", r.status, r.body)
	}
	if got, want := h.isMember(acme, h.alice.ID), rt.method == http.MethodPut; got != want {
		t.Errorf("alice is a member of acme: %t, want %t", got, want)
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

// isMember reports whether the user userID is a member of the org orgID.
func (h *testHub) isMember(orgID, userID string) bool {
	h.t.Helper()
	return h.count(`SELECT count(*) FROM memberships WHERE org_id = ? AND user_id = ?`, orgID, userID) == 1
}

// answeredOrgIDs returns the ids of the orgs an answer is about: the id of
// an Org object, or the org_id of each item of a member list.
func answeredOrgIDs(t *testing.T, r response) []string {
	t.Helper()
	if _, ok := r.json["items"]; ok {
		return itemMember(t, r, "org_id")
	}
	id, _ := r.json["id"].(string)
	return []string{id}
}

// itemMember returns the member name, a string, of each item of a list
// answer.
func itemMember(t *testing.T, r response, name string) []string {
	t.Helper()
	items, ok := r.json["items"].([]any)
	if !ok {
		t.Fatalf("answer = %d %s, want a list", r.status, r.body)
	}
	values := make([]string, 0, len(items))
	for _, it := range items {
		v, _ := it.(map[string]any)[name].(string)
		values = append(values, v)
	}
	return values
}

// newOrgBody is a POST /orgs body for an org no team hub has.
func newOrgBody(name, slug string) map[string]any {
	return map[string]any{"name": name, "slug": slug}
}

// itemNames returns the names of the items of a list answer.
func itemNames(t *testing.T, r response) []string {
	t.Helper()
	return itemMember(t, r, "name")
}

func TestCreateOrg_CreatesAnActiveOrgWithoutMembers(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for _, c := range []struct {
		name string
		body map[string]any
	}{
		{"name and slug", newOrgBody("Acme", "acme")},
		{"every member", map[string]any{"name": "  Beta Co ", "slug": "b", "url": "https://b.example",
			"owner_id": h.alice.ID}},
		{"a slug of 128 characters", newOrgBody("L", strings.Repeat("a", 128))},
		{"a slug with _ and -", newOrgBody("M", "a_-9")},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.advance(time.Hour)
			r := h.call(http.MethodPost, "/api/v1/orgs", h.admin, c.body)
			if r.status != http.StatusCreated {
				t.Fatalf("POST /orgs = %d %s, want 201", r.status, r.body)
			}
			if got := slices.Sorted(maps.Keys(r.json)); !slices.Equal(got, orgMembers) {
				t.Errorf("members = %v, want %v", got, orgMembers)
			}
			id, _ := r.json["id"].(string)
			if _, err := uuid.Parse(id); err != nil || len(id) != 36 {
				t.Errorf("id = %q, want a new UUID", id)
			}
			now := h.now().Format(time.RFC3339)
			want := map[string]any{"name": strings.TrimSpace(c.body["name"].(string)), "slug": c.body["slug"],
				"status": "active", "url": "", "owner_id": nil, "created_at": now, "updated_at": now}
			if v, ok := c.body["url"]; ok {
				want["url"] = v
			}
			if v, ok := c.body["owner_id"]; ok {
				want["owner_id"] = v
			}
			for member, w := range want {
				if r.json[member] != w {
					t.Errorf("%s = %v, want %v", member, r.json[member], w)
				}
			}
			if got := h.get("/api/v1/orgs/"+c.body["slug"].(string), h.admin).json; !reflect.DeepEqual(got, r.json) {
				t.Errorf("GET /orgs/%s = %v, want %v", c.body["slug"], got, r.json)
			}
			if m := h.membershipsOf(id); len(m) != 0 {
				t.Errorf("memberships of the new org = %+v, want none", m)
			}
		})
	}
}

func TestCreateOrg_RejectsAnInvalidBodyAndCreatesNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	valid := func(change func(map[string]any)) map[string]any {
		b := map[string]any{"name": "New", "slug": "new", "url": "https://new.example", "owner_id": h.alice.ID}
		change(b)
		return b
	}
	set := func(member string, v any) map[string]any {
		return valid(func(b map[string]any) { b[member] = v })
	}
	for name, body := range map[string]any{
		"no name":                  valid(func(b map[string]any) { delete(b, "name") }),
		"a blank name":             set("name", "   "),
		"a number as name":         set("name", 5),
		"no slug":                  valid(func(b map[string]any) { delete(b, "slug") }),
		"an empty slug":            set("slug", ""),
		"a slug of 129 characters": set("slug", strings.Repeat("a", 129)),
		"a slug in upper case":     set("slug", "Acme"),
		"a slug with a dot":        set("slug", "a.b"),
		"a slug starting with -":   set("slug", "-ab"),
		"a slug ending with _":     set("slug", "ab_"),
		"the slug _":               set("slug", "_"),
		"a number as url":          set("url", 5),
		"an owner_id of no user":   set("owner_id", uuid.NewV4().String()),
		"a username as owner_id":   set("owner_id", "alice"),
		"a number as owner_id":     set("owner_id", 5),
	} {
		t.Run(name, func(t *testing.T) {
			n := h.count(`SELECT count(*) FROM orgs`)
			before := h.snapshot()
			r := h.call(http.MethodPost, "/api/v1/orgs", h.admin, body)
			if r.status != http.StatusBadRequest || r.errorField("error_type") != "invalid_request" {
				t.Errorf("answer = %d %s, want 400 invalid_request", r.status, r.body)
			}
			if got := h.count(`SELECT count(*) FROM orgs`); got != n || h.snapshot() != before {
				t.Errorf("the store changed: %d orgs, want %d", got, n)
			}
		})
	}
}

func TestCreateOrg_RefusesATakenNameOrSlugAndCreatesNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.seedOrg("Acme Inc", "acme")
	for name, body := range map[string]map[string]any{
		"the name":                   newOrgBody("Acme Inc", "other"),
		"the name with white space":  newOrgBody(" Acme Inc ", "other"),
		"the slug":                   newOrgBody("Other", "acme"),
		"the name of a personal org": newOrgBody("alice", "other"),
	} {
		t.Run(name, func(t *testing.T) {
			n := h.count(`SELECT count(*) FROM orgs`)
			r := h.call(http.MethodPost, "/api/v1/orgs", h.admin, body)
			if r.status != http.StatusConflict || r.errorField("error_type") != "conflict" {
				t.Errorf("answer = %d %s, want 409 conflict", r.status, r.body)
			}
			if got := h.count(`SELECT count(*) FROM orgs`); got != n {
				t.Errorf("%d orgs, want %d", got, n)
			}
		})
	}
	t.Run("the name in other case", func(t *testing.T) {
		if r := h.call(http.MethodPost, "/api/v1/orgs", h.admin, newOrgBody("acme inc", "other")); r.status != http.StatusCreated {
			t.Errorf("answer = %d %s, want 201", r.status, r.body)
		}
	})
}

func TestUpdateOrg_ReplacesTheNameAndURLSentAndKeepsTheSlug(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.seedOrg("Acme Inc", "acme")
	// In order: each case starts from the org the case before left.
	for _, c := range []struct {
		name string
		body func(before map[string]any) map[string]any
	}{
		{"a name", func(map[string]any) map[string]any { return map[string]any{"name": "Acme 2"} }},
		{"a url", func(map[string]any) map[string]any { return map[string]any{"url": "https://a.example"} }},
		{"an empty url", func(map[string]any) map[string]any { return map[string]any{"url": ""} }},
		{"a name with white space and a url", func(map[string]any) map[string]any {
			return map[string]any{"name": " Acme 3 ", "url": "u"}
		}},
		{"a name and a slug", func(map[string]any) map[string]any {
			return map[string]any{"name": "Acme 4", "slug": "changed"}
		}},
		{"the current name", func(before map[string]any) map[string]any { return map[string]any{"name": before["name"]} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := h.get("/api/v1/orgs/acme", h.admin).json
			body := c.body(before)
			h.advance(time.Hour)
			r := h.call(http.MethodPatch, "/api/v1/orgs/acme", h.admin, body)
			if r.status != http.StatusOK {
				t.Fatalf("PATCH /orgs/acme = %d %s, want 200", r.status, r.body)
			}
			want := maps.Clone(before)
			if name, ok := body["name"].(string); ok {
				want["name"] = strings.TrimSpace(name)
			}
			if u, ok := body["url"]; ok {
				want["url"] = u
			}
			want["updated_at"] = h.now().Format(time.RFC3339)
			if !reflect.DeepEqual(r.json, want) {
				t.Errorf("org = %v, want %v", r.json, want)
			}
			if got := h.get("/api/v1/orgs/acme", h.admin).json; !reflect.DeepEqual(got, r.json) {
				t.Errorf("GET /orgs/acme = %v, want %v", got, r.json)
			}
		})
	}
}

func TestUpdateOrg_RejectsABodyWithoutAValidNameOrURL(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.seedOrg("Acme Inc", "acme")
	for name, body := range map[string]any{
		"an empty object":            map[string]any{},
		"a slug alone":               map[string]any{"slug": "x"},
		"an empty name":              map[string]any{"name": ""},
		"a blank name":               map[string]any{"name": "  "},
		"a number as name":           map[string]any{"name": 5},
		"a number as url":            map[string]any{"url": 5},
		"a name and a number as url": map[string]any{"name": "ok", "url": 5},
	} {
		t.Run(name, func(t *testing.T) {
			before := h.get("/api/v1/orgs/acme", h.admin).json
			r := h.call(http.MethodPatch, "/api/v1/orgs/acme", h.admin, body)
			if r.status != http.StatusBadRequest || r.errorField("error_type") != "invalid_request" {
				t.Errorf("answer = %d %s, want 400 invalid_request", r.status, r.body)
			}
			if got := h.get("/api/v1/orgs/acme", h.admin).json; !reflect.DeepEqual(got, before) {
				t.Errorf("GET /orgs/acme = %v, want %v", got, before)
			}
		})
	}
}

func TestUpdateOrg_RefusesTheNameOfAnotherOrgAndChangesNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.seedOrg("Acme Inc", "acme")
	h.seedOrg("Beta", "beta")
	for name, body := range map[string]any{
		"the name":                            map[string]any{"name": "Beta"},
		"the name with white space and a url": map[string]any{"name": " Beta ", "url": "new"},
	} {
		t.Run(name, func(t *testing.T) {
			before := h.get("/api/v1/orgs/acme", h.admin).json
			h.advance(time.Hour)
			r := h.call(http.MethodPatch, "/api/v1/orgs/acme", h.admin, body)
			if r.status != http.StatusConflict || r.errorField("error_type") != "conflict" {
				t.Errorf("answer = %d %s, want 409 conflict", r.status, r.body)
			}
			if got := h.get("/api/v1/orgs/acme", h.admin).json; !reflect.DeepEqual(got, before) {
				t.Errorf("GET /orgs/acme = %v, want %v", got, before)
			}
		})
	}
}

// newOrgListHub is a team hub with the active orgs alpha, Zed and beta
// beside the personal orgs of root and alice, and the blocked org
// blocked-co.
func newOrgListHub(t *testing.T) teamHub {
	t.Helper()
	h := newTeamHub(t)
	for _, name := range []string{"alpha", "Zed", "beta"} {
		h.seedOrg(name, strings.ToLower(name))
	}
	h.setOrgStatus(h.seedOrg("blocked-co", "blocked-co"), identity.StatusBlocked)
	return h
}

func TestListOrgs_ListsActiveOrgsByNameInPages(t *testing.T) {
	t.Parallel()
	h := newOrgListHub(t)
	r := h.get("/api/v1/orgs?limit=200", h.admin)
	if r.status != http.StatusOK {
		t.Fatalf("GET /orgs = %d %s, want 200", r.status, r.body)
	}
	for _, it := range r.json["items"].([]any) {
		if status := it.(map[string]any)["status"]; status != "active" {
			t.Errorf("an item has the status %v, want active", status)
		}
	}
	// Byte-wise, upper case comes before lower case.
	if got, want := itemNames(t, r), []string{"Zed", "alice", "alpha", "beta", "root"}; !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	ids := itemIDs(t, r)
	if got, requests := h.walk("/api/v1/orgs", h.admin, 2); !slices.Equal(got, ids) || requests != 3 {
		t.Errorf("pages of 2 = %v in %d requests, want %v in 3", got, requests, ids)
	}
}

func TestListOrgs_AddsBlockedOrgsForIncludeBlockedTrue(t *testing.T) {
	t.Parallel()
	h := newOrgListHub(t)
	r := h.get("/api/v1/orgs?include_blocked=true&limit=200", h.ka)
	if r.status != http.StatusOK {
		t.Fatalf("GET /orgs?include_blocked=true = %d %s, want 200", r.status, r.body)
	}
	if got, want := itemNames(t, r), []string{"Zed", "alice", "alpha", "beta", "blocked-co", "root"}; !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if n := h.count(`SELECT count(*) FROM orgs`); len(r.json["items"].([]any)) != n {
		t.Errorf("%d items, want every one of the %d orgs", len(r.json["items"].([]any)), n)
	}
	for _, it := range r.json["items"].([]any) {
		if item := it.(map[string]any); item["name"] == "blocked-co" && item["status"] != "blocked" {
			t.Errorf("blocked-co has the status %v, want blocked", item["status"])
		}
	}
	ids := itemIDs(t, r)
	if got, _ := h.walk("/api/v1/orgs?include_blocked=true", h.ka, 4); !slices.Equal(got, ids) {
		t.Errorf("pages of 4 = %v, want %v", got, ids)
	}
	for _, v := range []string{"false", "1"} {
		t.Run("include_blocked="+v, func(t *testing.T) {
			if names := itemNames(t, h.get("/api/v1/orgs?limit=200&include_blocked="+v, h.ka)); slices.Contains(names, "blocked-co") {
				t.Errorf("names = %v, want no blocked-co", names)
			}
		})
	}
}

func TestGetOrg_AnswersAnAdminAnyOrgAndAMemberAnActiveOne(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	acme := h.seedOrg("Acme Inc", "acme")
	blocked := h.seedOrg("Blocked Co", "blocked-co")
	h.setOrgStatus(blocked, identity.StatusBlocked)
	h.addMember(acme, h.alice.ID)
	h.addMember(blocked, h.alice.ID)
	for _, c := range []struct{ caller, callerName, slug, status string }{
		{h.admin, "admin token", "acme", "active"},
		{h.ka, "API key of an admin", "acme", "active"},
		{h.ku, "API key of a member", "acme", "active"},
		{h.pu, "PAT of a member", "acme", "active"},
		{h.ku, "API key of the owner", "alice", "active"},
		{h.admin, "admin token", "blocked-co", "blocked"},
		{h.ka, "API key of an admin", "blocked-co", "blocked"},
	} {
		t.Run(c.callerName+"/"+c.slug, func(t *testing.T) {
			r := h.get("/api/v1/orgs/"+c.slug, c.caller)
			if r.status != http.StatusOK {
				t.Fatalf("GET /orgs/%s = %d %s, want 200", c.slug, r.status, r.body)
			}
			if got := slices.Sorted(maps.Keys(r.json)); !slices.Equal(got, orgMembers) {
				t.Errorf("members = %v, want %v", got, orgMembers)
			}
			if r.json["slug"] != c.slug || r.json["status"] != c.status {
				t.Errorf("org = %s, want %s with the status %s", r.body, c.slug, c.status)
			}
		})
	}
}

func TestOrgSelectors_ReachTheSameOrgByIdOrSlug(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	id := h.seedOrg("Acme Inc", "acme")
	h.addMember(id, h.root.ID)
	o := h.get("/api/v1/orgs/acme", h.admin).json
	if got := h.get("/api/v1/orgs/"+id, h.admin).json; !reflect.DeepEqual(got, o) {
		t.Errorf("GET /orgs/%s = %v, want %v", id, got, o)
	}
	for _, rt := range orgSelectorRoutes {
		for _, sel := range []string{id, "acme"} {
			path := strings.ReplaceAll(rt.path, ":org", sel)
			t.Run(rt.method+" "+path, func(t *testing.T) {
				if membershipRoute(rt) {
					h.reachesAliceInAcme(t, rt, id, path)
					return
				}
				if orgDeleteRoute(rt) {
					h.deletesAcme(t, id, path)
					return
				}
				r := h.call(rt.method, path, h.admin, rt.body)
				if ids := answeredOrgIDs(t, r); r.status != http.StatusOK || len(ids) == 0 || slices.ContainsFunc(ids, func(got string) bool { return got != id }) {
					t.Errorf("answer = %d %s, want 200 about acme's id %s alone", r.status, r.body, id)
				}
			})
		}
	}
}

func TestOrgSelectors_AnswerNotFoundForNoOrgAndChangeNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.addMember(h.seedOrg("Acme Inc", "acme"), h.alice.ID)
	selectors := map[string]string{
		"a random UUID":          uuid.NewV4().String(),
		"an unknown slug":        "nope",
		"the slug in upper case": "ACME",
		"the name":               url.PathEscape("Acme Inc"),
	}
	for _, rt := range orgSelectorRoutes {
		for name, sel := range selectors {
			t.Run(fmt.Sprintf("%s %s/%s", rt.method, rt.path, name), func(t *testing.T) {
				before := h.snapshot()
				r := h.call(rt.method, strings.ReplaceAll(rt.path, ":org", sel), h.admin, rt.body)
				checkRefusal(t, "answer", r, http.StatusNotFound, "not_found", "org not found")
				if h.snapshot() != before {
					t.Errorf("the store changed")
				}
			})
		}
	}
}

// newMembersHub is a team hub with the org acme, whose members carol, alice
// and bob were added in that order a minute apart, after the users were
// created; the active org empty without members; and the blocked org
// blocked-co. It returns acme's id and the time alice was added.
func newMembersHub(t *testing.T) (h teamHub, acme string, aliceAdded time.Time) {
	t.Helper()
	h = newTeamHub(t)
	acme = h.seedOrg("Acme Inc", "acme")
	h.seedOrg("Empty", "empty")
	h.setOrgStatus(h.seedOrg("Blocked Co", "blocked-co"), identity.StatusBlocked)
	bob, carol := h.createUser("bob", identity.RoleUser), h.createUser("carol", identity.RoleUser)
	for _, u := range []identity.User{carol, h.alice, bob} {
		h.advance(time.Minute)
		h.addMember(acme, u.ID)
		if u.ID == h.alice.ID {
			aliceAdded = h.now()
		}
	}
	return h, acme, aliceAdded
}

func TestListMembers_ListsTheMembershipsByUsernameInPages(t *testing.T) {
	t.Parallel()
	h, acme, aliceAdded := newMembersHub(t)
	for name, caller := range map[string]string{"admin token": h.admin, "API key of an admin": h.ka,
		"API key of a member": h.ku, "PAT of a member": h.pu} {
		t.Run(name, func(t *testing.T) {
			r := h.get("/api/v1/orgs/acme/members", caller)
			if r.status != http.StatusOK {
				t.Fatalf("GET /orgs/acme/members = %d %s, want 200", r.status, r.body)
			}
			if got, want := itemMember(t, r, "username"), []string{"alice", "bob", "carol"}; !slices.Equal(got, want) {
				t.Fatalf("usernames = %v, want %v", got, want)
			}
			first := r.json["items"].([]any)[0].(map[string]any)
			if got := slices.Sorted(maps.Keys(first)); !slices.Equal(got, memberItemMembers) {
				t.Errorf("members = %v, want %v", got, memberItemMembers)
			}
			want := map[string]any{"org_id": acme, "user_id": h.alice.ID, "username": "alice",
				"email": "alice@example.com", "created_at": aliceAdded.Format(time.RFC3339)}
			if !reflect.DeepEqual(first, want) {
				t.Errorf("alice's item = %v, want %v", first, want)
			}
			if first["created_at"] == h.alice.CreatedAt.Format(time.RFC3339) {
				t.Errorf("created_at = %v, the time alice was created, want the time she was added", first["created_at"])
			}
		})
	}
	t.Run("pages", func(t *testing.T) {
		first := h.get("/api/v1/orgs/acme/members?limit=2", h.admin)
		cursor, ok := first.json["next_cursor"].(string)
		if got := itemMember(t, first, "username"); !ok || !slices.Equal(got, []string{"alice", "bob"}) {
			t.Fatalf("first page = %s, want alice and bob and a next_cursor", first.body)
		}
		next := h.get("/api/v1/orgs/acme/members?limit=2&cursor="+url.QueryEscape(cursor), h.admin)
		if got := itemMember(t, next, "username"); !slices.Equal(got, []string{"carol"}) || next.json["next_cursor"] != nil {
			t.Errorf("second page = %s, want carol and no next_cursor", next.body)
		}
	})
	t.Run("an org without members", func(t *testing.T) {
		r := h.get("/api/v1/orgs/empty/members", h.admin)
		if items, ok := r.json["items"].([]any); r.status != http.StatusOK || !ok || len(items) != 0 {
			t.Errorf("answer = %d %s, want 200 with items []", r.status, r.body)
		}
	})
	t.Run("a blocked org, for an admin", func(t *testing.T) {
		if r := h.get("/api/v1/orgs/blocked-co/members", h.admin); r.status != http.StatusOK {
			t.Errorf("answer = %d %s, want 200", r.status, r.body)
		}
	})
	t.Run("a bad limit is refused before the org is looked up", func(t *testing.T) {
		r := h.get("/api/v1/orgs/nope/members?limit=0", h.admin)
		checkRefusal(t, "GET /orgs/nope/members?limit=0", r, http.StatusBadRequest, "invalid_request",
			"limit must be an integer from 1 to 200")
	})
}

func TestAddMember_CreatesTheMembershipAtTheTimeOfTheRequest(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.seedOrg("Acme Inc", "acme")
	h.setOrgStatus(h.seedOrg("Blocked Co", "blocked-co"), identity.StatusBlocked)
	bob := h.createUser("bob", identity.RoleUser)
	h.createUser("carol", identity.RoleUser)
	h.setStatus(h.createUser("mallory", identity.RoleUser).ID, identity.StatusBlocked)
	for _, c := range []struct{ name, org, selector, username string }{
		{"a user by username", "acme", "alice", "alice"},
		{"a user by id", "acme", bob.ID, "bob"},
		{"a user by e-mail", "acme", "carol@example.com", "carol"},
		{"a blocked user", "acme", "mallory", "mallory"},
		{"a blocked org", "blocked-co", "alice", "alice"},
	} {
		t.Run(c.name, func(t *testing.T) {
			h.advance(time.Minute)
			r := h.call(http.MethodPut, "/api/v1/orgs/"+c.org+"/members/"+c.selector, h.admin, nil)
			if r.status != http.StatusNoContent || len(r.body) != 0 {
				t.Fatalf("answer = %d %s, want 204 without a body", r.status, r.body)
			}
			members := h.get("/api/v1/orgs/"+c.org+"/members", h.admin)
			i := slices.Index(itemMember(t, members, "username"), c.username)
			if i < 0 {
				t.Fatalf("members of %s = %s, want %s among them", c.org, members.body, c.username)
			}
			if got, want := members.json["items"].([]any)[i].(map[string]any)["created_at"], h.now().Format(time.RFC3339); got != want {
				t.Errorf("created_at = %v, want the time of the request %s", got, want)
			}
		})
	}
	if slugs := itemMember(t, h.get("/api/v1/user/orgs", h.ku), "slug"); !slices.Contains(slugs, "acme") {
		t.Errorf("alice's orgs = %v, want acme among them", slugs)
	}
	if r := h.get("/api/v1/orgs/acme", h.ku); r.status != http.StatusOK {
		t.Errorf("GET /orgs/acme by alice = %d %s, want 200", r.status, r.body)
	}
}

func TestAddMember_LeavesAnExistingMembershipAsItIs(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.addMember(h.seedOrg("Acme Inc", "acme"), h.alice.ID)
	before, stored := h.get("/api/v1/orgs/acme/members", h.admin).json["items"], h.snapshot()
	h.advance(2 * time.Second)
	if r := h.call(http.MethodPut, "/api/v1/orgs/acme/members/alice@example.com", h.admin, nil); r.status != http.StatusNoContent {
		t.Errorf("answer = %d %s, want 204", r.status, r.body)
	}
	if after := h.get("/api/v1/orgs/acme/members", h.admin).json["items"]; !reflect.DeepEqual(after, before) {
		t.Errorf("members = %v, want %v", after, before)
	}
	if h.snapshot() != stored {
		t.Errorf("the store changed")
	}
}

func TestRemoveMember_DeletesTheMembershipAndKeepsTheUserTheOrgAndItsOwner(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	if r := h.get("/api/v1/orgs/alice", h.ku); r.status != http.StatusOK {
		t.Fatalf("GET /orgs/alice by alice before = %d %s, want 200", r.status, r.body)
	}
	r := h.call(http.MethodDelete, "/api/v1/orgs/alice/members/alice", h.admin, nil)
	if r.status != http.StatusNoContent || len(r.body) != 0 {
		t.Fatalf("answer = %d %s, want 204 without a body", r.status, r.body)
	}
	if items := itemMember(t, h.get("/api/v1/orgs/alice/members", h.admin), "username"); len(items) != 0 {
		t.Errorf("members of alice = %v, want none", items)
	}
	checkRefusal(t, "GET /orgs/alice by alice", h.get("/api/v1/orgs/alice", h.ku), http.StatusNotFound, "not_found", "org not found")
	if o := h.get("/api/v1/orgs/alice", h.admin); o.status != http.StatusOK || o.json["owner_id"] != h.alice.ID {
		t.Errorf("GET /orgs/alice = %d %s, want 200 with owner_id %s", o.status, o.body, h.alice.ID)
	}
	if u := h.get("/api/v1/users/alice", h.admin); u.status != http.StatusOK {
		t.Errorf("GET /users/alice = %d %s, want 200", u.status, u.body)
	}
}

func TestRemoveMember_AnswersNotFoundForNoMembershipAndChangesNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.addMember(h.seedOrg("Acme Inc", "acme"), h.alice.ID)
	h.createUser("bob", identity.RoleUser)
	before := h.snapshot()
	r := h.call(http.MethodDelete, "/api/v1/orgs/acme/members/bob", h.admin, nil)
	checkRefusal(t, "DELETE of a user who is no member", r, http.StatusNotFound, "not_found", "membership not found")
	if h.snapshot() != before {
		t.Errorf("the store changed")
	}
	if r := h.call(http.MethodDelete, "/api/v1/orgs/acme/members/alice", h.admin, nil); r.status != http.StatusNoContent {
		t.Fatalf("DELETE of a member = %d %s, want 204", r.status, r.body)
	}
	r = h.call(http.MethodDelete, "/api/v1/orgs/acme/members/alice", h.admin, nil)
	checkRefusal(t, "DELETE of the membership just deleted", r, http.StatusNotFound, "not_found", "membership not found")
}

func TestOrgReads_AnswerOneNotFoundForAMissingAForeignAndABlockedOrg(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	blocked := h.seedOrg("Blocked Co", "blocked-co")
	h.addMember(blocked, h.alice.ID)
	h.setOrgStatus(blocked, identity.StatusBlocked)
	h.seedOrg("Other", "other")
	for _, route := range []string{"/api/v1/orgs/%s", "/api/v1/orgs/%s/members"} {
		for name, caller := range map[string]string{"API key of a user": h.ku, "PAT of a user": h.pu, "PAT of an admin": h.pa} {
			t.Run(route+"/"+name, func(t *testing.T) {
				missing := h.get(fmt.Sprintf(route, "nope"), caller)
				checkRefusal(t, "a missing org", missing, http.StatusNotFound, "not_found", "org not found")
				for what, slug := range map[string]string{"an org of which the caller is no member": "other",
					"a blocked org of which the caller is a member": "blocked-co"} {
					if caller == h.pa && slug == "blocked-co" {
						continue // root is no member of blocked-co
					}
					r := h.get(fmt.Sprintf(route, slug), caller)
					if r.status != missing.status || string(r.body) != string(missing.body) {
						t.Errorf("%s: answer = %d %s, want the answer for a missing org, %d %s", what, r.status, r.body,
							missing.status, missing.body)
					}
				}
			})
		}
	}
}

func TestBlockAndUnblockOrg_SetTheStatusAndKeepTheMembers(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	acme := h.seedOrg("Acme Inc", "acme")
	h.addMember(acme, h.alice.ID)
	listed := func(t *testing.T) (byAlice, byAdmin bool) {
		t.Helper()
		byAlice = slices.Contains(itemMember(t, h.get("/api/v1/user/orgs", h.ku), "slug"), "acme")
		byAdmin = slices.Contains(itemMember(t, h.get("/api/v1/orgs?limit=200", h.admin), "slug"), "acme")
		return byAlice, byAdmin
	}
	for _, c := range []struct {
		verb, caller, status string
		listed               bool
	}{
		{"block", h.admin, "blocked", false},
		{"unblock", h.ka, "active", true},
	} {
		t.Run(c.verb, func(t *testing.T) {
			h.advance(time.Minute)
			r := h.call(http.MethodPost, "/api/v1/orgs/acme/"+c.verb, c.caller, nil)
			if r.status != http.StatusOK {
				t.Fatalf("POST /orgs/acme/%s = %d %s, want 200", c.verb, r.status, r.body)
			}
			if got := slices.Sorted(maps.Keys(r.json)); !slices.Equal(got, orgMembers) {
				t.Errorf("members = %v, want %v", got, orgMembers)
			}
			if r.json["id"] != acme || r.json["status"] != c.status || r.json["updated_at"] != h.now().Format(time.RFC3339) {
				t.Errorf("org = %s, want acme with the status %s, updated at %s", r.body, c.status, h.now().Format(time.RFC3339))
			}
			if byAlice, byAdmin := listed(t); byAlice != c.listed || byAdmin != c.listed {
				t.Errorf("acme listed by alice's GET /user/orgs: %t, by GET /orgs: %t, want %t", byAlice, byAdmin, c.listed)
			}
			if got := itemMember(t, h.get("/api/v1/orgs/acme/members", h.admin), "username"); !slices.Equal(got, []string{"alice"}) {
				t.Errorf("members of acme = %v, want [alice]", got)
			}
		})
	}
}

func TestOrgStatusChanges_AnswerTheUnchangedOrgWhenNothingChanges(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.seedOrg("Acme Inc", "acme")
	h.setOrgStatus(h.seedOrg("Blocked Co", "blocked-co"), identity.StatusBlocked)
	for _, c := range []struct{ verb, slug string }{{"block", "blocked-co"}, {"unblock", "acme"}} {
		t.Run(c.verb+" "+c.slug, func(t *testing.T) {
			before, stored := h.get("/api/v1/orgs/"+c.slug, h.admin).json, h.snapshot()
			h.advance(2 * time.Second)
			r := h.call(http.MethodPost, "/api/v1/orgs/"+c.slug+"/"+c.verb, h.admin, nil)
			if r.status != http.StatusOK || !reflect.DeepEqual(r.json, before) {
				t.Errorf("answer = %d %s, want 200 with %v", r.status, r.body, before)
			}
			if h.snapshot() != stored {
				t.Errorf("the store changed")
			}
		})
	}
}

func TestDeleteOrg_DeletesTheOrgAndItsMembershipsAndKeepsTheUsers(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	bob := h.createUser("bob", identity.RoleUser)
	acme := h.seedOrg("Acme Inc", "acme")
	h.addMember(acme, h.alice.ID)
	h.addMember(acme, bob.ID)
	blocked := h.seedOrg("Blocked Co", "blocked-co")
	h.addMember(blocked, h.alice.ID)
	h.setOrgStatus(blocked, identity.StatusBlocked)
	for _, c := range []struct {
		name, slug string
		members    []string
	}{
		{"an org with members", "acme", []string{"alice", "bob"}},
		{"a personal org", "alice", []string{"alice"}},
		{"a blocked org", "blocked-co", []string{"alice"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			o := h.get("/api/v1/orgs/"+c.slug, h.admin).json
			id, name := o["id"].(string), o["name"].(string)
			if n := len(h.membershipsOf(id)); n != len(c.members) {
				t.Fatalf("%s has %d members before the delete, want %d", c.slug, n, len(c.members))
			}
			r := h.call(http.MethodDelete, "/api/v1/orgs/"+c.slug, h.admin, nil)
			if r.status != http.StatusNoContent || len(r.body) != 0 {
				t.Fatalf("DELETE /orgs/%s = %d %s, want 204 without a body", c.slug, r.status, r.body)
			}
			for _, sel := range []string{c.slug, id} {
				checkRefusal(t, "GET /orgs/"+sel, h.get("/api/v1/orgs/"+sel, h.admin), http.StatusNotFound, "not_found", "org not found")
			}
			if slugs := itemMember(t, h.get("/api/v1/user/orgs", h.ku), "slug"); slices.Contains(slugs, c.slug) {
				t.Errorf("alice's orgs = %v, want no %s", slugs, c.slug)
			}
			if ms := h.membershipsOf(id); len(ms) != 0 {
				t.Errorf("memberships of the deleted org = %v, want none", ms)
			}
			for _, member := range c.members {
				if u := h.get("/api/v1/users/"+member, h.admin); u.status != http.StatusOK {
					t.Errorf("GET /users/%s = %d %s, want 200", member, u.status, u.body)
				}
			}
			if r := h.call(http.MethodPost, "/api/v1/orgs", h.admin, newOrgBody(name, c.slug)); r.status != http.StatusCreated {
				t.Errorf("POST /orgs with the deleted org's name and slug = %d %s, want 201", r.status, r.body)
			}
		})
	}
}
