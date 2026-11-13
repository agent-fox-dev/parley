package identity_test

import (
	"fmt"
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
			r := wantStatus(t, "POST /orgs", h.call(http.MethodPost, "/api/v1/orgs", h.admin, c.body), http.StatusCreated)
			checkMembers(t, r.json, orgMembers)
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

func TestOrgChanges_StoreTheChangeAndTheTimeOfIt(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.addMember(h.seedOrg("Acme Inc", "acme"), h.alice.ID)
	// listed checks whether alice's orgs and the admin's org list hold acme,
	// and that acme kept its member.
	listed := func(want bool) func(*testing.T) {
		return func(t *testing.T) {
			byAlice := slices.Contains(itemValues(t, h.get("/api/v1/user/orgs", h.ku), "slug"), "acme")
			byAdmin := slices.Contains(itemValues(t, h.get("/api/v1/orgs?limit=200", h.admin), "slug"), "acme")
			if byAlice != want || byAdmin != want {
				t.Errorf("acme listed by alice's GET /user/orgs: %t, by GET /orgs: %t, want %t", byAlice, byAdmin, want)
			}
			if got := itemValues(t, h.get("/api/v1/orgs/acme/members", h.admin), "username"); !slices.Equal(got, []string{"alice"}) {
				t.Errorf("members of acme = %v, want [alice]", got)
			}
		}
	}
	const acme = "/api/v1/orgs/acme"
	patch := func(name string, body, changed map[string]any) recordChange {
		return recordChange{name: name, method: http.MethodPatch, path: acme, caller: h.admin, get: acme, body: body,
			changed: changed}
	}
	// In order: each change starts from the org the one before left.
	h.checkChanges(t, []recordChange{
		patch("a name", map[string]any{"name": "Acme 2"}, map[string]any{"name": "Acme 2"}),
		patch("a url", map[string]any{"url": "https://a.example"}, map[string]any{"url": "https://a.example"}),
		patch("an empty url", map[string]any{"url": ""}, map[string]any{"url": ""}),
		patch("a name with white space and a url", map[string]any{"name": " Acme 3 ", "url": "u"},
			map[string]any{"name": "Acme 3", "url": "u"}),
		patch("a name and a slug", map[string]any{"name": "Acme 4", "slug": "changed"}, map[string]any{"name": "Acme 4"}),
		patch("the current name", map[string]any{"name": "Acme 4"}, nil),
		{name: "block", method: http.MethodPost, path: acme + "/block", caller: h.admin, get: acme,
			changed: map[string]any{"status": "blocked"}, then: listed(false)},
		{name: "unblock", method: http.MethodPost, path: acme + "/unblock", caller: h.ka, get: acme,
			changed: map[string]any{"status": "active"}, then: listed(true)},
	})
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
	r := wantStatus(t, "GET /orgs", h.get("/api/v1/orgs?limit=200", h.admin), http.StatusOK)
	// Byte-wise, upper case comes before lower case.
	if got, want := itemValues(t, r, "name"), []string{"Zed", "alice", "alpha", "beta", "root"}; !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if got := itemValues(t, r, "status"); !slices.Equal(got, slices.Repeat([]string{"active"}, 5)) {
		t.Errorf("statuses = %v, want active alone", got)
	}
	ids := itemValues(t, r, "id")
	if got, requests := h.walkBy("/api/v1/orgs", h.admin, 2, "id"); !slices.Equal(got, ids) || requests != 3 {
		t.Errorf("pages of 2 = %v in %d requests, want %v in 3", got, requests, ids)
	}
}

func TestListOrgs_AddsBlockedOrgsForIncludeBlockedTrue(t *testing.T) {
	t.Parallel()
	h := newOrgListHub(t)
	r := wantStatus(t, "GET /orgs?include_blocked=true", h.get("/api/v1/orgs?include_blocked=true&limit=200", h.ka), http.StatusOK)
	if got, want := itemValues(t, r, "name"), []string{"Zed", "alice", "alpha", "beta", "blocked-co", "root"}; !slices.Equal(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
	if n := h.count(`SELECT count(*) FROM orgs`); len(r.json["items"].([]any)) != n {
		t.Errorf("%d items, want every one of the %d orgs", len(r.json["items"].([]any)), n)
	}
	if got, want := itemValues(t, r, "status"), []string{"active", "active", "active", "active", "blocked", "active"}; !slices.Equal(got, want) {
		t.Errorf("statuses = %v, want %v", got, want)
	}
	ids := itemValues(t, r, "id")
	if got, _ := h.walkBy("/api/v1/orgs?include_blocked=true", h.ka, 4, "id"); !slices.Equal(got, ids) {
		t.Errorf("pages of 4 = %v, want %v", got, ids)
	}
	for _, v := range []string{"false", "1"} {
		t.Run("include_blocked="+v, func(t *testing.T) {
			if names := itemValues(t, h.get("/api/v1/orgs?limit=200&include_blocked="+v, h.ka), "name"); slices.Contains(names, "blocked-co") {
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
			r := wantStatus(t, "GET /orgs/"+c.slug, h.get("/api/v1/orgs/"+c.slug, c.caller), http.StatusOK)
			checkMembers(t, r.json, orgMembers)
			if r.json["slug"] != c.slug || r.json["status"] != c.status {
				t.Errorf("org = %s, want %s with the status %s", r.body, c.slug, c.status)
			}
		})
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
			r := wantStatus(t, "GET /orgs/acme/members", h.get("/api/v1/orgs/acme/members", caller), http.StatusOK)
			if got, want := itemValues(t, r, "username"), []string{"alice", "bob", "carol"}; !slices.Equal(got, want) {
				t.Fatalf("usernames = %v, want %v", got, want)
			}
			first := r.json["items"].([]any)[0].(map[string]any)
			checkMembers(t, first, memberItemMembers)
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
		if got := itemValues(t, first, "username"); !ok || !slices.Equal(got, []string{"alice", "bob"}) {
			t.Fatalf("first page = %s, want alice and bob and a next_cursor", first.body)
		}
		next := h.get("/api/v1/orgs/acme/members?limit=2&cursor="+url.QueryEscape(cursor), h.admin)
		if got := itemValues(t, next, "username"); !slices.Equal(got, []string{"carol"}) || next.json["next_cursor"] != nil {
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
			i := slices.Index(itemValues(t, members, "username"), c.username)
			if i < 0 {
				t.Fatalf("members of %s = %s, want %s among them", c.org, members.body, c.username)
			}
			if got, want := members.json["items"].([]any)[i].(map[string]any)["created_at"], h.now().Format(time.RFC3339); got != want {
				t.Errorf("created_at = %v, want the time of the request %s", got, want)
			}
		})
	}
	if slugs := itemValues(t, h.get("/api/v1/user/orgs", h.ku), "slug"); !slices.Contains(slugs, "acme") {
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
	h.advance(2 * time.Second)
	if r := h.callUnchanged(t, http.MethodPut, "/api/v1/orgs/acme/members/alice@example.com", h.admin, nil); r.status != http.StatusNoContent {
		t.Errorf("answer = %d %s, want 204", r.status, r.body)
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
	if items := itemValues(t, h.get("/api/v1/orgs/alice/members", h.admin), "username"); len(items) != 0 {
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
	r := h.callUnchanged(t, http.MethodDelete, "/api/v1/orgs/acme/members/bob", h.admin, nil)
	checkRefusal(t, "DELETE of a user who is no member", r, http.StatusNotFound, "not_found", "membership not found")
	wantStatus(t, "DELETE of a member", h.call(http.MethodDelete, "/api/v1/orgs/acme/members/alice", h.admin, nil), http.StatusNoContent)
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
			if slugs := itemValues(t, h.get("/api/v1/user/orgs", h.ku), "slug"); slices.Contains(slugs, c.slug) {
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
