package identity_test

import (
	"cmp"
	"fmt"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"
	"uuid"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// userMembers are the members of the User object.
var userMembers = []string{"created_at", "email", "full_name", "id", "provider", "provider_id", "role", "status",
	"updated_at", "username"}

func TestCreateUser_CreatesAnActiveUserWithAPersonalOrg(t *testing.T) {
	t.Parallel()
	// No user has role admin, so the bootstrap e-mail would make a first
	// login an admin.
	h := newTestHub(t, withAdminEmail("boss@example.com"))
	for _, c := range []struct {
		name     string
		body     map[string]any
		fullName string
	}{
		{"with a full name", map[string]any{"username": "carol", "email": "carol@example.com", "provider": "github",
			"provider_id": "1", "full_name": "Carol C"}, "Carol C"},
		{"without a full name", map[string]any{"username": "dave", "email": "dave@example.com", "provider": "github",
			"provider_id": "2"}, ""},
		{"with the bootstrap admin e-mail", map[string]any{"username": "erin", "email": "boss@example.com",
			"provider": "google", "provider_id": "3"}, ""},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := wantStatus(t, "POST /users", h.call(http.MethodPost, "/api/v1/users", "Bearer "+h.adminToken, c.body),
				http.StatusCreated)
			checkMembers(t, r.json, userMembers)
			id, _ := r.json["id"].(string)
			if _, err := uuid.Parse(id); err != nil || len(id) != 36 {
				t.Errorf("id = %q, want a new UUID", id)
			}
			now := h.now().Format(time.RFC3339)
			for member, want := range map[string]any{
				"username": c.body["username"], "email": c.body["email"], "full_name": c.fullName, "role": "user",
				"status": "active", "provider": c.body["provider"], "provider_id": c.body["provider_id"],
				"created_at": now, "updated_at": now,
			} {
				if r.json[member] != want {
					t.Errorf("%s = %v, want %v", member, r.json[member], want)
				}
			}
			if stored := h.storedUser(id); stored.Role != "user" || stored.Status != "active" {
				t.Errorf("stored user = %+v, want role user and status active", stored)
			}
			username := c.body["username"].(string)
			o := h.orgBySlug(strings.ToLower(username))
			if o.Name != username || o.OwnerID != id || o.Status != "active" {
				t.Errorf("personal org = %+v, want name %s, owner %s and status active", o, username, id)
			}
			if m := h.membershipsOf(o.ID); len(m) != 1 || m[0].UserID != id {
				t.Errorf("memberships of the personal org = %+v, want the user's alone", m)
			}
		})
	}
}

func TestCreateUser_GivesThePersonalOrgTheSlugOfTheRuleOrTheFirstFreeNumberedOne(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	for _, c := range []struct {
		username string
		taken    []string
		want     string
	}{
		{"Alice.Smith", nil, "alice-smith"},
		{"42bob", nil, "u-42bob"},
		{"-_ab_-", nil, "ab"},
		{"a", nil, "u-<id8>"},
		{"Frank", []string{"frank"}, "frank-1"},
		{"gina", []string{"gina", "gina-1"}, "gina-2"},
		{"hal", []string{"hal", "hal-1", "hal-2", "hal-3", "hal-4", "hal-5", "hal-6", "hal-7", "hal-8", "hal-9"}, "hal-10"},
	} {
		t.Run(c.username, func(t *testing.T) {
			for _, s := range c.taken {
				h.seedOrg("Org "+s, s)
			}
			r := h.call(http.MethodPost, "/api/v1/users", "Bearer "+h.adminToken, newUserBody(c.username))
			id := wantStatus(t, "POST /users", r, http.StatusCreated).json["id"].(string)
			name, slug, _, _ := h.orgOf(id)
			if want := strings.ReplaceAll(c.want, "<id8>", id[:8]); slug != want || name != c.username {
				t.Errorf("personal org = %s %s, want %s %s", name, slug, c.username, want)
			}
		})
	}
}

func TestAdminWrites_RejectAnInvalidBodyAndChangeNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.seedOrg("Acme Inc", "acme")
	newUser := with(newUserBody("carol"), "full_name", "Carol C")
	userBodies := map[string]any{
		"an empty object":    map[string]any{},
		"username a number":  with(newUser, "username", 7),
		"full_name a number": with(newUser, "full_name", 7),
		"full_name null":     with(newUser, "full_name", nil),
	}
	for _, m := range []string{"username", "email", "provider", "provider_id"} {
		for what, v := range map[string]any{"missing": omitted{}, "empty": "", "null": nil} {
			userBodies[m+" "+what] = with(newUser, m, v)
		}
	}
	newOrg := map[string]any{"name": "New", "slug": "new", "url": "https://new.example", "owner_id": h.alice.ID}
	for _, rt := range []struct {
		method, path string
		bodies       map[string]any
	}{
		{http.MethodPost, "/api/v1/users", userBodies},
		{http.MethodPatch, "/api/v1/users/alice", map[string]any{
			"no full_name":       map[string]any{},
			"full_name a number": map[string]any{"full_name": 5},
			"full_name null":     map[string]any{"full_name": nil},
		}},
		{http.MethodPost, "/api/v1/orgs", map[string]any{
			"no name":                  with(newOrg, "name", omitted{}),
			"a blank name":             with(newOrg, "name", "   "),
			"a number as name":         with(newOrg, "name", 5),
			"no slug":                  with(newOrg, "slug", omitted{}),
			"an empty slug":            with(newOrg, "slug", ""),
			"a slug of 129 characters": with(newOrg, "slug", strings.Repeat("a", 129)),
			"a slug in upper case":     with(newOrg, "slug", "Acme"),
			"a slug with a dot":        with(newOrg, "slug", "a.b"),
			"a slug starting with -":   with(newOrg, "slug", "-ab"),
			"a slug ending with _":     with(newOrg, "slug", "ab_"),
			"the slug _":               with(newOrg, "slug", "_"),
			"a number as url":          with(newOrg, "url", 5),
			"an owner_id of no user":   with(newOrg, "owner_id", uuid.NewV4().String()),
			"a username as owner_id":   with(newOrg, "owner_id", "alice"),
			"a number as owner_id":     with(newOrg, "owner_id", 5),
		}},
		{http.MethodPatch, "/api/v1/orgs/acme", map[string]any{
			"an empty object":            map[string]any{},
			"a slug alone":               map[string]any{"slug": "x"},
			"an empty name":              map[string]any{"name": ""},
			"a blank name":               map[string]any{"name": "  "},
			"a number as name":           map[string]any{"name": 5},
			"a number as url":            map[string]any{"url": 5},
			"a name and a number as url": map[string]any{"name": "ok", "url": 5},
		}},
	} {
		for name, body := range rt.bodies {
			t.Run(rt.method+" "+rt.path+"/"+name, func(t *testing.T) {
				r := h.callUnchanged(t, rt.method, rt.path, h.admin, body)
				if r.status != http.StatusBadRequest || r.errorField("error_type") != "invalid_request" {
					t.Errorf("answer = %d %s, want 400 invalid_request", r.status, r.body)
				}
			})
		}
	}
}

func TestAdminWrites_RefuseWhatIsTakenAndChangeNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for _, o := range [][2]string{{"Acme Inc", "acme"}, {"Beta", "beta"}, {"henry", "henrys-org"}, {"Org ivy", "ivy"}} {
		h.seedOrg(o[0], o[1])
	}
	for i := 1; i <= 10; i++ {
		h.seedOrg(fmt.Sprintf("Org ivy %d", i), fmt.Sprintf("ivy-%d", i))
	}
	user := func(username, email, providerID string) map[string]any {
		return map[string]any{"username": username, "email": email, "provider": "github", "provider_id": providerID}
	}
	for _, c := range []struct {
		name, method, path string
		body               map[string]any
	}{
		{"the username", http.MethodPost, "/api/v1/users", user("alice", "new@example.com", "new")},
		{"the e-mail", http.MethodPost, "/api/v1/users", user("new", "alice@example.com", "new")},
		{"the provider identity", http.MethodPost, "/api/v1/users", user("new", "new@example.com", "alice")},
		{"an org has the username as its name", http.MethodPost, "/api/v1/users", newUserBody("henry")},
		{"no numbered slug is free", http.MethodPost, "/api/v1/users", newUserBody("ivy")},
		{"the name", http.MethodPost, "/api/v1/orgs", newOrgBody("Acme Inc", "other")},
		{"the name with white space", http.MethodPost, "/api/v1/orgs", newOrgBody(" Acme Inc ", "other")},
		{"the slug", http.MethodPost, "/api/v1/orgs", newOrgBody("Other", "acme")},
		{"the name of a personal org", http.MethodPost, "/api/v1/orgs", newOrgBody("alice", "other")},
		{"the name of another org", http.MethodPatch, "/api/v1/orgs/acme", map[string]any{"name": "Beta"}},
		{"that name with white space and a url", http.MethodPatch, "/api/v1/orgs/acme", map[string]any{"name": " Beta ", "url": "new"}},
	} {
		t.Run(c.method+" "+c.path+"/"+c.name, func(t *testing.T) {
			h.advance(time.Hour)
			r := h.callUnchanged(t, c.method, c.path, h.admin, c.body)
			if r.status != http.StatusConflict || r.errorField("error_type") != "conflict" {
				t.Errorf("answer = %d %s, want 409 conflict", r.status, r.body)
			}
		})
	}
	for name, c := range map[string]struct {
		path string
		body map[string]any
	}{
		"a new provider id of a taken provider": {"/api/v1/users", user("new", "new@example.com", "alice-2")},
		"an org name in other case":             {"/api/v1/orgs", newOrgBody("acme inc", "other")},
	} {
		t.Run(name, func(t *testing.T) {
			wantStatus(t, "POST "+c.path, h.call(http.MethodPost, c.path, h.admin, c.body), http.StatusCreated)
		})
	}
}

// newListHub is a bootstrapped test hub with five active users created an
// hour apart and mallory, blocked, created at the same instant as the third
// of them. It returns them all newest first: created_at descending, then
// id descending.
func newListHub(t *testing.T) (*testHub, []identity.User) {
	t.Helper()
	h := newTestHub(t)
	var users []identity.User
	for i, name := range []string{"ann", "ben", "cat", "dan", "eve"} {
		h.advance(time.Hour)
		users = append(users, h.createUser(name, identity.RoleUser))
		if i == 2 {
			mallory := h.createUser("mallory", identity.RoleUser)
			h.setStatus(mallory.ID, identity.StatusBlocked)
			mallory.Status = identity.StatusBlocked
			users = append(users, mallory)
		}
	}
	slices.SortFunc(users, func(a, b identity.User) int {
		return cmp.Or(b.CreatedAt.Compare(a.CreatedAt), cmp.Compare(b.ID, a.ID))
	})
	return h, users
}

// idsOf returns the ids of the users whose status is one of statuses.
func idsOf(users []identity.User, statuses ...string) []string {
	var ids []string
	for _, u := range users {
		if slices.Contains(statuses, u.Status) {
			ids = append(ids, u.ID)
		}
	}
	return ids
}

func TestListUsers_ListsActiveUsersNewestFirstInPages(t *testing.T) {
	t.Parallel()
	h, users := newListHub(t)
	admin := "Bearer " + h.adminToken
	want := idsOf(users, identity.StatusActive)
	r := wantStatus(t, "GET /users", h.get("/api/v1/users", admin), http.StatusOK)
	if got := itemValues(t, r, "status"); !slices.Equal(got, slices.Repeat([]string{"active"}, len(want))) {
		t.Errorf("statuses = %v, want active alone", got)
	}
	if got := itemValues(t, r, "id"); !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	if got, requests := h.walkBy("/api/v1/users", admin, 2, "id"); !slices.Equal(got, want) || requests != 3 {
		t.Errorf("pages of 2 = %v in %d requests, want %v in 3", got, requests, want)
	}
	for _, v := range []string{"false", "1", "yes", "TRUE", ""} {
		t.Run("include_blocked="+v, func(t *testing.T) {
			if got := itemValues(t, h.get("/api/v1/users?include_blocked="+v, admin), "id"); !slices.Equal(got, want) {
				t.Errorf("items = %v, want %v", got, want)
			}
		})
	}
}

func TestListUsers_AddsBlockedUsersForIncludeBlockedTrue(t *testing.T) {
	t.Parallel()
	h, users := newListHub(t)
	admin := "Bearer " + h.adminToken
	want := idsOf(users, identity.StatusActive, identity.StatusBlocked)
	if n := h.count(`SELECT count(*) FROM users`); len(want) != n {
		t.Fatalf("the hub has %d users, the test %d", n, len(want))
	}
	r := h.get("/api/v1/users?include_blocked=true", admin)
	if got := itemValues(t, r, "id"); !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	var statuses []string
	for _, u := range users {
		statuses = append(statuses, u.Status)
	}
	if got := itemValues(t, r, "status"); !slices.Equal(got, statuses) {
		t.Errorf("statuses = %v, want %v, mallory's alone blocked", got, statuses)
	}
	// Pages of 3 split mallory from the user created at the same instant.
	if got, _ := h.walkBy("/api/v1/users?include_blocked=true", admin, 3, "id"); !slices.Equal(got, want) {
		t.Errorf("pages of 3 = %v, want %v", got, want)
	}
}

func TestGetUser_AnswersAnActiveOrABlockedUser(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.setStatus(h.createUser("mallory", identity.RoleUser).ID, identity.StatusBlocked)
	for _, c := range []struct{ username, status string }{{"alice", "active"}, {"mallory", "blocked"}} {
		t.Run(c.username, func(t *testing.T) {
			r := wantStatus(t, "GET /users/"+c.username, h.get("/api/v1/users/"+c.username, h.admin), http.StatusOK)
			checkMembers(t, r.json, userMembers)
			if r.json["username"] != c.username || r.json["status"] != c.status {
				t.Errorf("user = %s, want %s with the status %s", r.body, c.username, c.status)
			}
		})
	}
}

func TestUserChanges_StoreTheChangeAndTheTimeOfIt(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	// credentials checks that no credential is revoked and that GET /user
	// answers alice's API key and PAT status.
	credentials := func(status int) func(*testing.T) {
		return func(t *testing.T) {
			if n := h.count(`SELECT (SELECT count(*) FROM api_keys WHERE revoked_at IS NOT NULL) +
(SELECT count(*) FROM pats WHERE revoked_at IS NOT NULL)`); n != 0 {
				t.Errorf("%d credentials are revoked, want none", n)
			}
			for name, cred := range map[string]string{"API key": h.ku, "PAT": h.pu} {
				if got := h.get("/api/v1/user", cred); got.status != status {
					t.Errorf("GET /user with alice's %s = %d %s, want %d", name, got.status, got.body, status)
				}
			}
		}
	}
	const alice = "/api/v1/users/alice"
	// In order: the empty full name clears the one set before it, root's
	// API key demotes alice while root is an active admin, and alice's
	// credentials are refused while she is blocked and pass again once she
	// is unblocked.
	h.checkChanges(t, []recordChange{
		{name: "a full name", method: http.MethodPatch, path: alice, caller: h.ka, get: alice,
			body: map[string]any{"full_name": "Alice B."}, changed: map[string]any{"full_name": "Alice B."}},
		{name: "an empty full name", method: http.MethodPatch, path: alice, caller: h.ka, get: alice,
			body: map[string]any{"full_name": ""}, changed: map[string]any{"full_name": ""}},
		{name: "promote", method: http.MethodPost, path: alice + "/promote", caller: h.admin, get: alice,
			changed: map[string]any{"role": "admin"}},
		{name: "demote", method: http.MethodPost, path: alice + "/demote", caller: h.ka, get: alice,
			changed: map[string]any{"role": "user"}},
		{name: "block", method: http.MethodPost, path: alice + "/block", caller: h.admin, get: alice,
			changed: map[string]any{"status": "blocked"}, then: credentials(http.StatusForbidden)},
		{name: "unblock", method: http.MethodPost, path: alice + "/unblock", caller: h.admin, get: alice,
			changed: map[string]any{"status": "active"}, then: credentials(http.StatusOK)},
	})
}

func TestStatusChanges_AnswerTheUnchangedRecordWhenNothingChanges(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.setStatus(h.createUser("mallory", identity.RoleUser).ID, identity.StatusBlocked)
	h.seedOrg("Acme Inc", "acme")
	h.setOrgStatus(h.seedOrg("Blocked Co", "blocked-co"), identity.StatusBlocked)
	for _, path := range []string{"/api/v1/users/root/promote", "/api/v1/users/alice/demote", "/api/v1/users/mallory/block",
		"/api/v1/users/alice/unblock", "/api/v1/orgs/blocked-co/block", "/api/v1/orgs/acme/unblock"} {
		t.Run(path, func(t *testing.T) {
			before := h.get(path[:strings.LastIndex(path, "/")], h.admin).json
			h.advance(2 * time.Second)
			if r := h.callUnchanged(t, http.MethodPost, path, h.admin, nil); r.status != http.StatusOK || !reflect.DeepEqual(r.json, before) {
				t.Errorf("answer = %d %s, want 200 with the unchanged %v", r.status, r.body, before)
			}
		})
	}
}

func TestLastAdminGuard_RefusesToDemoteOrDeleteTheOnlyActiveAdmin(t *testing.T) {
	t.Parallel()
	for _, blockedAdmin := range []bool{false, true} {
		t.Run(fmt.Sprintf("beside a blocked admin: %t", blockedAdmin), func(t *testing.T) {
			t.Parallel()
			h := newTeamHub(t)
			if blockedAdmin {
				h.setStatus(h.createUser("oscar", identity.RoleAdmin).ID, identity.StatusBlocked)
			}
			for _, rt := range []testRoute{{http.MethodPost, "/api/v1/users/root/demote", nil}, {http.MethodDelete, "/api/v1/users/root", nil}} {
				for name, caller := range map[string]string{"admin token": h.admin, "API key of root": h.ka} {
					r := h.callUnchanged(t, rt.method, rt.path, caller, nil)
					checkRefusal(t, rt.method+" "+rt.path+" by the "+name, r, http.StatusConflict, "conflict",
						"the user is the only active admin")
				}
			}
			if !blockedAdmin {
				return
			}
			if r := h.call(http.MethodPost, "/api/v1/users/oscar/demote", h.admin, nil); r.status != http.StatusOK || r.json["role"] != "user" {
				t.Errorf("POST /users/oscar/demote = %d %s, want 200 with the role user", r.status, r.body)
			}
		})
	}
}

func TestBlock_BlocksTheOnlyActiveAdmin(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	r := h.call(http.MethodPost, "/api/v1/users/root/block", h.admin, nil)
	if r.status != http.StatusOK || r.json["status"] != "blocked" || r.json["role"] != "admin" {
		t.Fatalf("POST /users/root/block = %d %s, want 200 with the status blocked and the role admin", r.status, r.body)
	}
	wantStatus(t, "GET /users with the admin token", h.get("/api/v1/users", h.admin), http.StatusOK)
	r = h.call(http.MethodPost, "/api/v1/users/root/unblock", h.admin, nil)
	if r.status != http.StatusOK || r.json["status"] != "active" {
		t.Fatalf("POST /users/root/unblock = %d %s, want 200 with the status active", r.status, r.body)
	}
	wantStatus(t, "GET /users with root's API key after the unblock", h.get("/api/v1/users", h.ka), http.StatusOK)
}

func TestDeleteUser_DeletesTheUserWhateverTheRoleAndStatus(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.setStatus(h.createUser("mallory", identity.RoleUser).ID, identity.StatusBlocked)
	h.setStatus(h.createUser("oscar", identity.RoleAdmin).ID, identity.StatusBlocked)
	h.createUser("ruth", identity.RoleAdmin)
	// root stays an active admin throughout.
	for _, c := range []struct{ name, username string }{
		{"a user", "alice"},
		{"a blocked user", "mallory"},
		{"a blocked admin", "oscar"},
		{"an active admin beside another", "ruth"},
	} {
		t.Run(c.name, func(t *testing.T) {
			u := h.get("/api/v1/users/"+c.username, h.admin).json
			r := h.call(http.MethodDelete, "/api/v1/users/"+c.username, h.admin, nil)
			if r.status != http.StatusNoContent || len(r.body) != 0 {
				t.Fatalf("DELETE /users/%s = %d %s, want 204 without a body", c.username, r.status, r.body)
			}
			id := u["id"].(string)
			for _, sel := range []string{id, u["email"].(string), c.username} {
				checkRefusal(t, "GET /users/"+sel, h.get("/api/v1/users/"+sel, h.admin), http.StatusNotFound, "not_found",
					"user not found")
			}
			if ids := itemValues(t, h.get("/api/v1/users?include_blocked=true&limit=200", h.admin), "id"); slices.Contains(ids, id) {
				t.Errorf("GET /users?include_blocked=true lists the deleted user %s", id)
			}
		})
	}
}

func TestDeleteUser_KeepsTheOrgsWithoutTheUserAsOwnerOrMember(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	bob := h.createUser("bob", identity.RoleUser)
	acme := h.seedOrg("Acme Inc", "acme")
	h.exec(`UPDATE orgs SET owner_id = ? WHERE id = ?`, h.alice.ID, acme)
	h.addMember(acme, h.alice.ID)
	h.addMember(acme, bob.ID)
	h.advance(time.Hour)
	wantStatus(t, "DELETE /users/alice", h.call(http.MethodDelete, "/api/v1/users/alice", h.admin, nil), http.StatusNoContent)
	now := h.now().Format(time.RFC3339)
	for _, slug := range []string{"alice", "acme"} {
		r := h.get("/api/v1/orgs/"+slug, h.admin)
		owner, present := r.json["owner_id"]
		if r.status != http.StatusOK || !present || owner != nil || r.json["updated_at"] != now {
			t.Errorf("GET /orgs/%s = %d %s, want 200 with owner_id null and updated_at %s", slug, r.status, r.body, now)
		}
	}
	if got := itemValues(t, h.get("/api/v1/orgs/acme/members", h.admin), "username"); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("members of acme = %v, want [bob]", got)
	}
	if got := itemValues(t, h.get("/api/v1/orgs/alice/members", h.admin), "username"); len(got) != 0 {
		t.Errorf("members of alice's personal org = %v, want none", got)
	}
	if got := h.get("/api/v1/orgs/bob", h.admin).json["owner_id"]; got != bob.ID {
		t.Errorf("owner_id of bob's personal org = %v, want %s", got, bob.ID)
	}
}

func TestDeleteUser_DeletesEveryCredentialOfTheUser(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	bound, _ := h.seedPAT(identity.PATSpec{UserID: h.alice.ID, Permissions: everyScope, Workspace: "docs"})
	if n := h.count(`SELECT count(*) FROM pats WHERE user_id = ? AND workspace IS NOT NULL`, h.alice.ID); n != 1 {
		t.Fatalf("alice has %d bound PATs, want 1", n)
	}
	wantStatus(t, "DELETE /users/alice", h.call(http.MethodDelete, "/api/v1/users/alice", h.admin, nil), http.StatusNoContent)
	for name, cred := range map[string]string{"API key": h.ku, "PAT": h.pu, "bound PAT": "Bearer " + bound} {
		checkRefusal(t, "GET /user with alice's "+name, h.get("/api/v1/user", cred), http.StatusUnauthorized,
			"unauthenticated", "invalid credentials")
	}
	if n := h.count(`SELECT (SELECT count(*) FROM api_keys WHERE user_id = ?) + (SELECT count(*) FROM pats WHERE user_id = ?)`,
		h.alice.ID, h.alice.ID); n != 0 {
		t.Errorf("%d API keys and PATs of alice are stored, want none", n)
	}
	for name, cred := range map[string]string{"API key": h.ka, "PAT": h.pa} {
		if r := h.get("/api/v1/user", cred); r.status != http.StatusOK {
			t.Errorf("GET /user with root's %s = %d %s, want 200", name, r.status, r.body)
		}
	}
}

func TestLastAdminGuard_RefusesOneOfTwoConcurrentChangesOfTheLastTwoAdmins(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name         string
		method, path string // the change of root
		rootWins     int    // the status of root's change when it is not refused
	}{
		{"demote root and demote ruth", http.MethodPost, "/api/v1/users/root/demote", http.StatusOK},
		{"delete root and demote ruth", http.MethodDelete, "/api/v1/users/root", http.StatusNoContent},
	} {
		for i := range 25 {
			t.Run(fmt.Sprintf("%s/%d", c.name, i), func(t *testing.T) {
				t.Parallel()
				h := newTestHub(t)
				h.createUser("root", identity.RoleAdmin)
				h.createUser("ruth", identity.RoleAdmin)
				admin := "Bearer " + h.adminToken
				var root, ruth response
				start := make(chan struct{})
				var wg sync.WaitGroup
				wg.Go(func() { <-start; root = h.call(c.method, c.path, admin, nil) })
				wg.Go(func() { <-start; ruth = h.call(http.MethodPost, "/api/v1/users/ruth/demote", admin, nil) })
				close(start)
				wg.Wait()
				refused := root
				switch {
				case root.status == c.rootWins && ruth.status == http.StatusConflict:
					refused = ruth
				case root.status == http.StatusConflict && ruth.status == http.StatusOK:
				default:
					t.Fatalf("answers = %d %s and %d %s, want one change done and the other 409", root.status, root.body,
						ruth.status, ruth.body)
				}
				checkRefusal(t, "the refused change", refused, http.StatusConflict, "conflict", "the user is the only active admin")
				if n := h.count(`SELECT count(*) FROM users WHERE role = ? AND status = ?`, identity.RoleAdmin,
					identity.StatusActive); n != 1 {
					t.Errorf("%d active admins remain, want 1", n)
				}
			})
		}
	}
}
