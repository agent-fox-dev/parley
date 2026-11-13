package identity_test

import (
	"cmp"
	"fmt"
	"maps"
	"net/http"
	"net/url"
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

// postUser posts body to POST /users as the caller authorization.
func (h *testHub) postUser(authorization string, body any) response {
	h.t.Helper()
	return h.call(http.MethodPost, "/api/v1/users", authorization, body)
}

func TestCreateUser_CreatesAnActiveUserWithAPersonalOrg(t *testing.T) {
	t.Parallel()
	// No user has role admin, so the bootstrap e-mail would make a first
	// login an admin.
	h := newTestHub(t, withAdminEmail("boss@example.com"))
	admin := "Bearer " + h.adminToken
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
			r := h.postUser(admin, c.body)
			if r.status != http.StatusCreated {
				t.Fatalf("status = %d %s, want 201", r.status, r.body)
			}
			if got := slices.Sorted(maps.Keys(r.json)); !slices.Equal(got, userMembers) {
				t.Errorf("members = %v, want %v", got, userMembers)
			}
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
			stored := h.storedUser(id)
			if stored.Role != "user" || stored.Status != "active" {
				t.Errorf("stored user = %+v, want role user and status active", stored)
			}
			username := c.body["username"].(string)
			o := h.orgBySlug(strings.ToLower(username))
			if o.Name != username || o.OwnerID != id || o.Status != "active" {
				t.Errorf("personal org = %+v, want name %s, owner %s and status active", o, username, id)
			}
			m := h.membershipsOf(o.ID)
			if len(m) != 1 || m[0].UserID != id {
				t.Errorf("memberships of the personal org = %+v, want the user's alone", m)
			}
		})
	}
}

func TestCreateUser_GivesThePersonalOrgTheSlugOfTheRule(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	admin := "Bearer " + h.adminToken
	for i, c := range []struct{ username, want string }{
		{"Alice.Smith", "alice-smith"},
		{"42bob", "u-42bob"},
		{"-_ab_-", "ab"},
		{"a", "u-<id8>"},
	} {
		t.Run(c.username, func(t *testing.T) {
			body := map[string]any{"username": c.username, "email": fmt.Sprintf("user%d@example.com", i),
				"provider": "github", "provider_id": fmt.Sprint(i)}
			r := h.postUser(admin, body)
			if r.status != http.StatusCreated {
				t.Fatalf("status = %d %s, want 201", r.status, r.body)
			}
			id := r.json["id"].(string)
			name, slug, _, _ := h.orgOf(id)
			if want := strings.ReplaceAll(c.want, "<id8>", id[:8]); slug != want || name != c.username {
				t.Errorf("personal org = %s %s, want %s %s", name, slug, c.username, want)
			}
		})
	}
}

func TestCreateUser_GivesThePersonalOrgTheFirstFreeNumberedSlug(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	admin := "Bearer " + h.adminToken
	for _, c := range []struct {
		username string
		taken    []string
		want     string
	}{
		{"Frank", []string{"frank"}, "frank-1"},
		{"gina", []string{"gina", "gina-1"}, "gina-2"},
		{"hal", []string{"hal", "hal-1", "hal-2", "hal-3", "hal-4", "hal-5", "hal-6", "hal-7", "hal-8", "hal-9"}, "hal-10"},
	} {
		t.Run(c.username, func(t *testing.T) {
			for _, s := range c.taken {
				h.seedOrg("Org "+s, s)
			}
			r := h.postUser(admin, newUserBody(c.username))
			if r.status != http.StatusCreated {
				t.Fatalf("status = %d %s, want 201", r.status, r.body)
			}
			if name, slug, _, _ := h.orgOf(r.json["id"].(string)); slug != c.want || name != c.username {
				t.Errorf("personal org = %s %s, want %s %s", name, slug, c.username, c.want)
			}
		})
	}
}

func TestCreateUser_RejectsABodyWithoutTheRequiredStrings(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	valid := func(change func(map[string]any)) map[string]any {
		b := newUserBody("carol")
		b["full_name"] = "Carol C"
		change(b)
		return b
	}
	bodies := map[string]any{
		"empty object":       map[string]any{},
		"username a number":  valid(func(b map[string]any) { b["username"] = 7 }),
		"full_name a number": valid(func(b map[string]any) { b["full_name"] = 7 }),
		"full_name null":     valid(func(b map[string]any) { b["full_name"] = nil }),
	}
	for _, m := range []string{"username", "email", "provider", "provider_id"} {
		bodies[m+" missing"] = valid(func(b map[string]any) { delete(b, m) })
		bodies[m+" empty"] = valid(func(b map[string]any) { b[m] = "" })
		bodies[m+" null"] = valid(func(b map[string]any) { b[m] = nil })
	}
	for name, body := range bodies {
		t.Run(name, func(t *testing.T) {
			before := h.snapshot()
			r := h.postUser(h.admin, body)
			if r.status != http.StatusBadRequest || r.errorField("error_type") != "invalid_request" {
				t.Errorf("answer = %d %s, want 400 invalid_request", r.status, r.body)
			}
			if h.snapshot() != before {
				t.Errorf("the store changed")
			}
		})
	}
}

func TestCreateUser_RefusesWhatIsTakenAndCreatesNothing(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name  string
		setup func(h teamHub)
		body  map[string]any
		// free is true when no user has the body's username.
		free bool
	}{
		{"the username is taken", nil,
			map[string]any{"username": "alice", "email": "new@example.com", "provider": "github", "provider_id": "new"}, false},
		{"the e-mail is taken", nil,
			map[string]any{"username": "new", "email": "alice@example.com", "provider": "github", "provider_id": "new"}, true},
		{"the provider identity is taken", nil,
			map[string]any{"username": "new", "email": "new@example.com", "provider": "github", "provider_id": "alice"}, true},
		{"an org has the username as its name", func(h teamHub) { h.seedOrg("henry", "henrys-org") },
			newUserBody("henry"), true},
		{"no numbered slug is free", func(h teamHub) {
			h.seedOrg("Org ivy", "ivy")
			for i := 1; i <= 10; i++ {
				h.seedOrg(fmt.Sprintf("Org ivy %d", i), fmt.Sprintf("ivy-%d", i))
			}
		}, newUserBody("ivy"), true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newTeamHub(t)
			if c.setup != nil {
				c.setup(h)
			}
			before := h.snapshot()
			r := h.postUser(h.admin, c.body)
			if r.status != http.StatusConflict || r.errorField("error_type") != "conflict" {
				t.Errorf("answer = %d %s, want 409 conflict", r.status, r.body)
			}
			if h.snapshot() != before {
				t.Errorf("the store changed")
			}
			if username := c.body["username"].(string); c.free {
				checkRefusal(t, "GET /users/"+username, h.get("/api/v1/users/"+username, h.admin), http.StatusNotFound,
					"not_found", "user not found")
			}
		})
	}
	t.Run("a new provider id of a taken provider", func(t *testing.T) {
		t.Parallel()
		h := newTeamHub(t)
		r := h.postUser(h.admin, map[string]any{"username": "new", "email": "new@example.com", "provider": "github",
			"provider_id": "alice-2"})
		if r.status != http.StatusCreated {
			t.Errorf("answer = %d %s, want 201", r.status, r.body)
		}
	})
}

// userSelectorRoutes are the admin routes that name a user by a selector,
// ":user" in their path, each with a body it accepts (nil for none). The
// membership routes name the org acme, which the selector tests seed. The
// user delete comes last, so that the routes before it find the user as the
// team hub created her.
var userSelectorRoutes = []adminRoute{
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

// userDeleteRoute reports whether rt deletes the user.
func userDeleteRoute(rt adminRoute) bool {
	return rt.method == http.MethodDelete && rt.path == "/api/v1/users/:user"
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

// itemIDs returns the ids of the items of a list answer.
func itemIDs(t *testing.T, r response) []string {
	t.Helper()
	items, ok := r.json["items"].([]any)
	if !ok {
		t.Fatalf("answer = %d %s, want a list", r.status, r.body)
	}
	ids := make([]string, 0, len(items))
	for _, it := range items {
		id, _ := it.(map[string]any)["id"].(string)
		ids = append(ids, id)
	}
	return ids
}

// walk reads the list at path page by page, limit items a page, following
// next_cursor until it is null, and returns the ids of every item in order
// and how many requests it took.
func (h *testHub) walk(path, authorization string, limit int) (ids []string, requests int) {
	h.t.Helper()
	sep := "?"
	if strings.Contains(path, "?") {
		sep = "&"
	}
	cursor := ""
	for requests < 10 {
		page := fmt.Sprintf("%s%slimit=%d", path, sep, limit)
		if cursor != "" {
			page += "&cursor=" + url.QueryEscape(cursor)
		}
		r := h.get(page, authorization)
		requests++
		if r.status != http.StatusOK {
			h.t.Fatalf("GET %s = %d %s, want 200", page, r.status, r.body)
		}
		ids = append(ids, itemIDs(h.t, r)...)
		next, ok := r.json["next_cursor"].(string)
		if !ok {
			return ids, requests
		}
		cursor = next
	}
	h.t.Fatalf("GET %s: next_cursor still set after %d pages", path, requests)
	return nil, 0
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
	r := h.get("/api/v1/users", admin)
	if r.status != http.StatusOK {
		t.Fatalf("GET /users = %d %s, want 200", r.status, r.body)
	}
	for _, it := range r.json["items"].([]any) {
		if status := it.(map[string]any)["status"]; status != "active" {
			t.Errorf("an item has the status %v, want active", status)
		}
	}
	if got := itemIDs(t, r); !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	if got, requests := h.walk("/api/v1/users", admin, 2); !slices.Equal(got, want) || requests != 3 {
		t.Errorf("pages of 2 = %v in %d requests, want %v in 3", got, requests, want)
	}
	for _, v := range []string{"false", "1", "yes", "TRUE", ""} {
		t.Run("include_blocked="+v, func(t *testing.T) {
			if got := itemIDs(t, h.get("/api/v1/users?include_blocked="+v, admin)); !slices.Equal(got, want) {
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
	if got := itemIDs(t, r); !slices.Equal(got, want) {
		t.Errorf("items = %v, want %v", got, want)
	}
	var blocked []any
	for _, it := range r.json["items"].([]any) {
		if item := it.(map[string]any); item["status"] == "blocked" {
			blocked = append(blocked, item["username"])
		}
	}
	if !slices.Equal(blocked, []any{"mallory"}) {
		t.Errorf("blocked items = %v, want mallory alone", blocked)
	}
	// Pages of 3 split mallory from the user created at the same instant.
	if got, _ := h.walk("/api/v1/users?include_blocked=true", admin, 3); !slices.Equal(got, want) {
		t.Errorf("pages of 3 = %v, want %v", got, want)
	}
}

func TestGetUser_AnswersAnActiveOrABlockedUser(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	mallory := h.createUser("mallory", identity.RoleUser)
	h.setStatus(mallory.ID, identity.StatusBlocked)
	for _, c := range []struct{ username, status string }{{"alice", "active"}, {"mallory", "blocked"}} {
		t.Run(c.username, func(t *testing.T) {
			r := h.get("/api/v1/users/"+c.username, h.admin)
			if r.status != http.StatusOK {
				t.Fatalf("GET /users/%s = %d %s, want 200", c.username, r.status, r.body)
			}
			if got := slices.Sorted(maps.Keys(r.json)); !slices.Equal(got, userMembers) {
				t.Errorf("members = %v, want %v", got, userMembers)
			}
			if r.json["username"] != c.username || r.json["status"] != c.status {
				t.Errorf("user = %s, want %s with status %s", r.body, c.username, c.status)
			}
		})
	}
}

func TestUpdateUser_StoresTheFullNameAndTheTimeOfTheChange(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	// In order: the empty string clears the name the first case set.
	for _, fullName := range []string{"Alice B.", ""} {
		t.Run(fmt.Sprintf("%q", fullName), func(t *testing.T) {
			h.advance(time.Hour)
			r := h.call(http.MethodPatch, "/api/v1/users/alice", h.ka, map[string]any{"full_name": fullName})
			if r.status != http.StatusOK {
				t.Fatalf("PATCH /users/alice = %d %s, want 200", r.status, r.body)
			}
			if r.json["id"] != h.alice.ID || r.json["full_name"] != fullName {
				t.Errorf("user = %s, want alice with the full name %q", r.body, fullName)
			}
			if want := h.now().Format(time.RFC3339); r.json["updated_at"] != want {
				t.Errorf("updated_at = %v, want %s", r.json["updated_at"], want)
			}
			if got := h.get("/api/v1/users/alice", h.admin).json; !reflect.DeepEqual(got, r.json) {
				t.Errorf("GET /users/alice = %v, want %v", got, r.json)
			}
		})
	}
}

func TestUpdateUser_RejectsABodyWithoutAFullNameString(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	for name, body := range map[string]any{
		"no full_name":       map[string]any{},
		"full_name a number": map[string]any{"full_name": 5},
		"full_name null":     map[string]any{"full_name": nil},
	} {
		t.Run(name, func(t *testing.T) {
			before := h.get("/api/v1/users/alice", h.admin).json
			r := h.call(http.MethodPatch, "/api/v1/users/alice", h.admin, body)
			if r.status != http.StatusBadRequest || r.errorField("error_type") != "invalid_request" {
				t.Errorf("answer = %d %s, want 400 invalid_request", r.status, r.body)
			}
			if got := h.get("/api/v1/users/alice", h.admin).json; !reflect.DeepEqual(got, before) {
				t.Errorf("GET /users/alice = %v, want %v", got, before)
			}
		})
	}
}

func TestUserSelectors_ReachTheSameUserByIdEmailOrUsername(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	id, acme := h.alice.ID, h.seedOrg("Acme Inc", "acme")
	for _, rt := range userSelectorRoutes {
		for _, sel := range []string{id, "alice@example.com", "alice"} {
			path := strings.ReplaceAll(rt.path, ":user", sel)
			t.Run(rt.method+" "+path, func(t *testing.T) {
				if membershipRoute(rt) {
					h.reachesAliceInAcme(t, rt, acme, path)
					return
				}
				if userDeleteRoute(rt) {
					h.deletesAlice(t, path)
					return
				}
				r := h.call(rt.method, path, h.admin, rt.body)
				if r.status != http.StatusOK || r.json["id"] != id {
					t.Fatalf("answer = %d %s, want 200 with alice's id %s", r.status, r.body, id)
				}
				if rt.method != http.MethodGet {
					return
				}
				if want := h.get("/api/v1/users/"+id, h.admin).json; !reflect.DeepEqual(r.json, want) {
					t.Errorf("user = %v, want %v", r.json, want)
				}
			})
		}
	}
}

func TestUserSelectors_AnswerNotFoundForNoUserAndChangeNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	h.addMember(h.seedOrg("Acme Inc", "acme"), h.alice.ID)
	selectors := map[string]string{
		"a random UUID":            uuid.NewV4().String(),
		"an unknown e-mail":        "nobody@example.com",
		"an unknown username":      "nobody",
		"a username in other case": "ALICE",
		"an e-mail in upper case":  "ALICE@EXAMPLE.COM",
	}
	for _, rt := range userSelectorRoutes {
		for name, sel := range selectors {
			t.Run(rt.method+" "+rt.path+"/"+name, func(t *testing.T) {
				before := h.snapshot()
				r := h.call(rt.method, strings.ReplaceAll(rt.path, ":user", sel), h.admin, rt.body)
				checkRefusal(t, "answer", r, http.StatusNotFound, "not_found", "user not found")
				if h.snapshot() != before {
					t.Errorf("the store changed")
				}
			})
		}
	}
}

// post sends POST path without a body, with authorization as the
// Authorization header.
func (h *testHub) post(path, authorization string) response {
	h.t.Helper()
	return h.call(http.MethodPost, path, authorization, nil)
}

func TestPromoteAndDemote_SetTheRoleAndTheTimeOfTheChange(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	before := withoutMembers(h.get("/api/v1/users/alice", h.admin).json, "role", "updated_at")
	// In order: root's API key demotes alice while root is an active admin.
	for _, c := range []struct{ verb, caller, role string }{
		{"promote", h.admin, "admin"},
		{"demote", h.ka, "user"},
	} {
		t.Run(c.verb, func(t *testing.T) {
			h.advance(time.Hour)
			r := h.post("/api/v1/users/alice/"+c.verb, c.caller)
			if r.status != http.StatusOK || r.json["role"] != c.role {
				t.Fatalf("POST /users/alice/%s = %d %s, want 200 with the role %s", c.verb, r.status, r.body, c.role)
			}
			if want := h.now().Format(time.RFC3339); r.json["updated_at"] != want {
				t.Errorf("updated_at = %v, want %s", r.json["updated_at"], want)
			}
			if got := withoutMembers(r.json, "role", "updated_at"); !reflect.DeepEqual(got, before) {
				t.Errorf("user = %v, want %v but for role and updated_at", got, before)
			}
			if got := h.get("/api/v1/users/alice", h.admin).json; !reflect.DeepEqual(got, r.json) {
				t.Errorf("GET /users/alice = %v, want %v", got, r.json)
			}
		})
	}
}

func TestBlockAndUnblock_SetTheStatusAndRevokeNoCredential(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	before := withoutMembers(h.get("/api/v1/users/alice", h.admin).json, "status", "updated_at")
	revoked := func() int {
		return h.count(`SELECT count(*) FROM api_keys WHERE revoked_at IS NOT NULL`) +
			h.count(`SELECT count(*) FROM pats WHERE revoked_at IS NOT NULL`)
	}
	// In order: alice's credentials are refused while she is blocked and
	// pass again once she is unblocked.
	for _, c := range []struct {
		verb, status string
		credentials  int
	}{
		{"block", "blocked", http.StatusForbidden},
		{"unblock", "active", http.StatusOK},
	} {
		t.Run(c.verb, func(t *testing.T) {
			h.advance(time.Hour)
			r := h.post("/api/v1/users/alice/"+c.verb, h.admin)
			if r.status != http.StatusOK || r.json["status"] != c.status || r.json["role"] != "user" {
				t.Fatalf("POST /users/alice/%s = %d %s, want 200 with the status %s and the role user", c.verb,
					r.status, r.body, c.status)
			}
			if want := h.now().Format(time.RFC3339); r.json["updated_at"] != want {
				t.Errorf("updated_at = %v, want %s", r.json["updated_at"], want)
			}
			if got := withoutMembers(r.json, "status", "updated_at"); !reflect.DeepEqual(got, before) {
				t.Errorf("user = %v, want %v but for status and updated_at", got, before)
			}
			if n := revoked(); n != 0 {
				t.Errorf("%d credentials are revoked, want none", n)
			}
			for name, cred := range map[string]string{"API key": h.ku, "PAT": h.pu} {
				if got := h.get("/api/v1/user", cred); got.status != c.credentials {
					t.Errorf("GET /user with alice's %s = %d %s, want %d", name, got.status, got.body, c.credentials)
				}
			}
		})
	}
}

func TestRoleAndStatusChanges_AnswerTheUnchangedUserWhenNothingChanges(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	mallory := h.createUser("mallory", identity.RoleUser)
	h.setStatus(mallory.ID, identity.StatusBlocked)
	for _, c := range []struct{ verb, username string }{
		{"promote", "root"}, {"demote", "alice"}, {"block", "mallory"}, {"unblock", "alice"},
	} {
		t.Run(c.verb+" "+c.username, func(t *testing.T) {
			before := h.get("/api/v1/users/"+c.username, h.admin).json
			stored := h.snapshot()
			h.advance(2 * time.Second)
			r := h.post("/api/v1/users/"+c.username+"/"+c.verb, h.admin)
			if r.status != http.StatusOK {
				t.Fatalf("POST /users/%s/%s = %d %s, want 200", c.username, c.verb, r.status, r.body)
			}
			if !reflect.DeepEqual(r.json, before) {
				t.Errorf("user = %v, want the unchanged %v", r.json, before)
			}
			if h.snapshot() != stored {
				t.Errorf("the store changed")
			}
		})
	}
}

func TestDemote_RefusesTheOnlyActiveAdmin(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		name string
		// blockedAdmin is true when oscar, a blocked admin, exists.
		blockedAdmin bool
	}{
		{"root is the only admin", false},
		{"root and a blocked admin exist", true},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			h := newTeamHub(t)
			if c.blockedAdmin {
				oscar := h.createUser("oscar", identity.RoleAdmin)
				h.setStatus(oscar.ID, identity.StatusBlocked)
			}
			for name, caller := range map[string]string{"admin token": h.admin, "API key of root": h.ka} {
				before := h.snapshot()
				r := h.post("/api/v1/users/root/demote", caller)
				checkRefusal(t, name, r, http.StatusConflict, "conflict", "the user is the only active admin")
				if got := h.get("/api/v1/users/root", h.admin).json["role"]; got != "admin" {
					t.Errorf("%s: root's role = %v, want admin", name, got)
				}
				if h.snapshot() != before {
					t.Errorf("%s: the store changed", name)
				}
			}
			if c.blockedAdmin {
				if r := h.post("/api/v1/users/oscar/demote", h.admin); r.status != http.StatusOK || r.json["role"] != "user" {
					t.Errorf("POST /users/oscar/demote = %d %s, want 200 with the role user", r.status, r.body)
				}
			}
		})
	}
}

func TestBlock_BlocksTheOnlyActiveAdmin(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	r := h.post("/api/v1/users/root/block", h.admin)
	if r.status != http.StatusOK || r.json["status"] != "blocked" || r.json["role"] != "admin" {
		t.Fatalf("POST /users/root/block = %d %s, want 200 with the status blocked and the role admin", r.status, r.body)
	}
	if got := h.get("/api/v1/users", h.admin); got.status != http.StatusOK {
		t.Errorf("GET /users with the admin token = %d %s, want 200", got.status, got.body)
	}
	r = h.post("/api/v1/users/root/unblock", h.admin)
	if r.status != http.StatusOK || r.json["status"] != "active" {
		t.Fatalf("POST /users/root/unblock = %d %s, want 200 with the status active", r.status, r.body)
	}
	if got := h.get("/api/v1/users", h.ka); got.status != http.StatusOK {
		t.Errorf("GET /users with root's API key after the unblock = %d %s, want 200", got.status, got.body)
	}
}

// deleteUser sends DELETE /users/<selector> as the admin token.
func (h *testHub) deleteUser(selector string) response {
	h.t.Helper()
	return h.call(http.MethodDelete, "/api/v1/users/"+selector, "Bearer "+h.adminToken, nil)
}

func TestDeleteUser_DeletesTheUserWhateverTheRoleAndStatus(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	mallory := h.createUser("mallory", identity.RoleUser)
	h.setStatus(mallory.ID, identity.StatusBlocked)
	oscar := h.createUser("oscar", identity.RoleAdmin)
	h.setStatus(oscar.ID, identity.StatusBlocked)
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
			r := h.deleteUser(c.username)
			if r.status != http.StatusNoContent || len(r.body) != 0 {
				t.Fatalf("DELETE /users/%s = %d %s, want 204 without a body", c.username, r.status, r.body)
			}
			id := u["id"].(string)
			for _, sel := range []string{id, u["email"].(string), c.username} {
				checkRefusal(t, "GET /users/"+sel, h.get("/api/v1/users/"+sel, h.admin), http.StatusNotFound, "not_found",
					"user not found")
			}
			if ids := itemIDs(t, h.get("/api/v1/users?include_blocked=true&limit=200", h.admin)); slices.Contains(ids, id) {
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
	if r := h.deleteUser("alice"); r.status != http.StatusNoContent {
		t.Fatalf("DELETE /users/alice = %d %s, want 204", r.status, r.body)
	}
	now := h.now().Format(time.RFC3339)
	for _, slug := range []string{"alice", "acme"} {
		r := h.get("/api/v1/orgs/"+slug, h.admin)
		owner, present := r.json["owner_id"]
		if r.status != http.StatusOK || !present || owner != nil || r.json["updated_at"] != now {
			t.Errorf("GET /orgs/%s = %d %s, want 200 with owner_id null and updated_at %s", slug, r.status, r.body, now)
		}
	}
	if got := itemMember(t, h.get("/api/v1/orgs/acme/members", h.admin), "username"); !slices.Equal(got, []string{"bob"}) {
		t.Errorf("members of acme = %v, want [bob]", got)
	}
	if got := itemMember(t, h.get("/api/v1/orgs/alice/members", h.admin), "username"); len(got) != 0 {
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
	if r := h.deleteUser("alice"); r.status != http.StatusNoContent {
		t.Fatalf("DELETE /users/alice = %d %s, want 204", r.status, r.body)
	}
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

func TestDeleteUser_RefusesTheOnlyActiveAdminAndDeletesNothing(t *testing.T) {
	t.Parallel()
	h := newTeamHub(t)
	oscar := h.createUser("oscar", identity.RoleAdmin)
	h.setStatus(oscar.ID, identity.StatusBlocked)
	for name, caller := range map[string]string{"admin token": h.admin, "API key of root itself": h.ka} {
		t.Run(name, func(t *testing.T) {
			before := h.snapshot()
			r := h.call(http.MethodDelete, "/api/v1/users/root", caller, nil)
			checkRefusal(t, "answer", r, http.StatusConflict, "conflict", "the user is the only active admin")
			if h.snapshot() != before {
				t.Errorf("the store changed")
			}
			if got := h.get("/api/v1/users", h.ka); got.status != http.StatusOK {
				t.Errorf("GET /users with root's API key = %d %s, want 200", got.status, got.body)
			}
		})
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
				wg.Go(func() { <-start; ruth = h.post("/api/v1/users/ruth/demote", admin) })
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
