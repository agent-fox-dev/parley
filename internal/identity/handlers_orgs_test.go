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

// orgSelectorRoutes are the admin routes that name an org by a selector,
// ":org" in their path, each with a body it accepts (nil for none).
var orgSelectorRoutes = []adminRoute{
	{http.MethodGet, "/api/v1/orgs/:org", nil},
	{http.MethodPatch, "/api/v1/orgs/:org", map[string]any{"url": "u"}},
}

// newOrgBody is a POST /orgs body for an org no team hub has.
func newOrgBody(name, slug string) map[string]any {
	return map[string]any{"name": name, "slug": slug}
}

// itemNames returns the names of the items of a list answer.
func itemNames(t *testing.T, r response) []string {
	t.Helper()
	items, ok := r.json["items"].([]any)
	if !ok {
		t.Fatalf("answer = %d %s, want a list", r.status, r.body)
	}
	names := make([]string, 0, len(items))
	for _, it := range items {
		name, _ := it.(map[string]any)["name"].(string)
		names = append(names, name)
	}
	return names
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
	h.seedOrg("Acme Inc", "acme")
	o := h.get("/api/v1/orgs/acme", h.admin).json
	id, _ := o["id"].(string)
	if got := h.get("/api/v1/orgs/"+id, h.admin).json; !reflect.DeepEqual(got, o) {
		t.Errorf("GET /orgs/%s = %v, want %v", id, got, o)
	}
	for _, rt := range orgSelectorRoutes {
		for _, sel := range []string{id, "acme"} {
			path := strings.ReplaceAll(rt.path, ":org", sel)
			t.Run(rt.method+" "+path, func(t *testing.T) {
				if r := h.call(rt.method, path, h.admin, rt.body); r.status != http.StatusOK || r.json["id"] != id {
					t.Errorf("answer = %d %s, want 200 with acme's id %s", r.status, r.body, id)
				}
			})
		}
	}
}

func TestOrgSelectors_AnswerNotFoundForNoOrgAndChangeNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.seedOrg("Acme Inc", "acme")
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
