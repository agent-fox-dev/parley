package identity_test

import (
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"uuid"
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
			if n := h.count(`SELECT count(*) FROM users WHERE username = ?`, c.body["username"]); c.free && n != 0 {
				t.Errorf("%d users have the username %v, want none", n, c.body["username"])
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
