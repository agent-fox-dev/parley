package identity_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// loginBody is a callback body the provider stub accepts.
var loginBody = map[string]any{"provider": "github", "code": "c", "redirect_uri": "http://127.0.0.1:5000/callback"}

// newLoginHub is a test hub with github configured whose provider answers
// the identity id.
func newLoginHub(t *testing.T, id identity.Identity, opts ...hubOption) *testHub {
	t.Helper()
	opts = append(opts, withProviders(config.Provider{Name: "github", ClientID: "gh-id", ClientSecret: "gh-secret"}))
	h := newTestHub(t, opts...)
	h.provider.set(id, nil)
	return h
}

// newIdentity is a github identity no user has.
func newIdentity(username, email string) identity.Identity {
	return identity.Identity{Provider: "github", ProviderID: "123", Username: username, FullName: "Alice A", Email: email}
}

// orgOf returns the name, slug, status and member ids of the org owned by
// userID.
func (h *testHub) orgOf(userID string) (name, slug, status string, members []string) {
	h.t.Helper()
	ctx, db := context.Background(), h.st.Main()
	var id string
	err := db.QueryRowContext(ctx, `SELECT id, name, slug, status FROM orgs WHERE owner_id = ?`, userID).Scan(&id, &name, &slug, &status)
	if err != nil {
		h.t.Fatalf("read the org of %s: %v", userID, err)
	}
	rows, err := db.QueryContext(ctx, `SELECT user_id FROM memberships WHERE org_id = ?`, id)
	if err != nil {
		h.t.Fatalf("read the members of %s: %v", id, err)
	}
	defer rows.Close()
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			h.t.Fatalf("read a member of %s: %v", id, err)
		}
		members = append(members, m)
	}
	return name, slug, status, members
}

func TestCallback_CreatesTheUserThePersonalOrgAndAKeyAtAFirstLogin(t *testing.T) {
	t.Parallel()
	cases := []struct{ username, slug string }{
		{"alice", "alice"},
		{"Alice.Smith", "alice-smith"},
	}
	for _, tc := range cases {
		t.Run(tc.username, func(t *testing.T) {
			t.Parallel()
			h := newLoginHub(t, newIdentity(tc.username, "a@x"))
			t0 := timefmt.Format(h.now())

			r := h.callback(loginBody)

			if r.status != 200 {
				t.Fatalf("status = %d, want 200; body %s", r.status, r.body)
			}
			u, _ := r.json["user"].(map[string]any)
			want := map[string]any{"username": tc.username, "email": "a@x", "full_name": "Alice A", "role": "user",
				"status": "active", "provider": "github", "provider_id": "123", "created_at": t0, "updated_at": t0}
			for k, v := range want {
				if u[k] != v {
					t.Errorf("user.%s = %v, want %v", k, u[k], v)
				}
			}
			userID, _ := u["id"].(string)
			key, _ := r.json["api_key"].(map[string]any)
			if got := slices.Sorted(maps.Keys(key)); !slices.Equal(got, []string{"expires_at", "key", "key_id"}) {
				t.Errorf("api_key members = %v, want expires_at, key, key_id", got)
			}
			keyID, _ := key["key_id"].(string)
			secret, ok := key["key"].(string)
			if !ok || len(secret) != len("parley_")+len(keyID)+1+32 || secret[:len("parley_")+len(keyID)+1] != "parley_"+keyID+"_" {
				t.Errorf("api_key.key = %v, want parley_%s_<32 characters>", key["key"], keyID)
			}
			if want := timefmt.Format(h.now().Add(90 * 24 * time.Hour)); key["expires_at"] != want {
				t.Errorf("api_key.expires_at = %v, want %s", key["expires_at"], want)
			}
			if n := h.count(`SELECT count(*) FROM orgs`); n != 1 {
				t.Errorf("%d orgs, want 1", n)
			}
			name, slug, status, members := h.orgOf(userID)
			if name != tc.username || slug != tc.slug || status != "active" || !slices.Equal(members, []string{userID}) {
				t.Errorf("org = name %q, slug %q, status %q, members %v; want %q, %q, active, [%s]",
					name, slug, status, members, tc.username, tc.slug, userID)
			}
			if n := h.count(`SELECT count(*) FROM api_keys WHERE user_id = ? AND revoked_at IS NULL`, userID); n != 1 {
				t.Errorf("%d live keys, want 1", n)
			}
			w := h.get("/api/v1/test/whoami", "Bearer "+secret)
			if wu, _ := w.json["user"].(map[string]any); w.status != 200 || wu["id"] != userID {
				t.Errorf("whoami with the new key = %d %s, want 200 as user %s", w.status, w.body, userID)
			}
		})
	}
}

func TestCallback_GivesANewUserTheFirstFreeUsernameSuffix(t *testing.T) {
	t.Parallel()
	cases := []struct {
		taken []string
		want  string
	}{
		{[]string{"alice"}, "alice-2"},
		{[]string{"alice", "alice-2", "alice-3"}, "alice-4"},
		{[]string{"alice", "alice-2", "alice-3", "alice-4", "alice-5", "alice-6", "alice-7", "alice-8", "alice-9"}, "alice-10"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			h := newLoginHub(t, newIdentity("alice", "new@x"))
			for _, name := range tc.taken {
				h.seedUser(name)
			}

			r := h.callback(loginBody)

			if r.status != 200 {
				t.Fatalf("status = %d, want 200; body %s", r.status, r.body)
			}
			u, _ := r.json["user"].(map[string]any)
			if u["username"] != tc.want {
				t.Errorf("username = %v, want %s", u["username"], tc.want)
			}
			if name, _, _, _ := h.orgOf(fmt.Sprint(u["id"])); name != tc.want {
				t.Errorf("org name = %q, want %s", name, tc.want)
			}
		})
	}
}

func TestCallback_MakesTheFirstUserWithTheAdminEmailAnAdmin(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		opts        []hubOption
		email       string
		adminStatus string // the status of an admin user that exists already; "" for none
		want        string
	}{
		{"admin e-mail in another letter case, no admin yet", []hubOption{withAdminEmail("Boss@X.io")}, "boss@x.io", "", "admin"},
		{"an admin exists", []hubOption{withAdminEmail("Boss@X.io")}, "boss@x.io", "active", "user"},
		{"a blocked admin exists", []hubOption{withAdminEmail("Boss@X.io")}, "boss@x.io", "blocked", "user"},
		{"another e-mail", []hubOption{withAdminEmail("boss@x.io")}, "a@x", "", "user"},
		{"no admin e-mail stored", []hubOption{notBootstrapped()}, "boss@x.io", "", "user"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLoginHub(t, newIdentity("alice", tc.email), tc.opts...)
			if tc.adminStatus != "" {
				admin := h.seedUser("root")
				h.exec(`UPDATE users SET role = 'admin', status = ? WHERE id = ?`, tc.adminStatus, admin.ID)
			}

			r := h.callback(loginBody)

			if r.status != 200 {
				t.Fatalf("status = %d, want 200; body %s", r.status, r.body)
			}
			u, _ := r.json["user"].(map[string]any)
			if u["role"] != tc.want {
				t.Errorf("role = %v, want %s", u["role"], tc.want)
			}
			stored, err := identity.UserByID(context.Background(), h.st.Main(), fmt.Sprint(u["id"]))
			if err != nil || stored.Role != tc.want {
				t.Errorf("stored role = %q (%v), want %s", stored.Role, err, tc.want)
			}
		})
	}
}

func TestCallback_RefusesAFirstLoginThatBreaksUniquenessAndCreatesNothing(t *testing.T) {
	t.Parallel()
	suffixed := func(name string, from int) []string {
		out := []string{name}
		for i := from; i <= 10; i++ {
			out = append(out, fmt.Sprintf("%s-%d", name, i))
		}
		return out
	}
	cases := []struct {
		name  string
		prior func(h *testHub)
	}{
		{"the e-mail is another user's", func(h *testHub) {
			bob := h.seedUser("bob")
			h.exec(`UPDATE users SET email = 'a@x' WHERE id = ?`, bob.ID)
		}},
		{"an org has the username as its name", func(h *testHub) { h.seedOrg("alice", "other") }},
		{"every username is taken", func(h *testHub) {
			for _, name := range suffixed("alice", 2) {
				h.seedUser(name)
			}
		}},
		{"every slug is taken", func(h *testHub) {
			for i, slug := range suffixed("alice", 1) {
				h.seedOrg(fmt.Sprintf("org%d", i), slug)
			}
		}},
	}
	counts := func(h *testHub) []int {
		var out []int
		for _, q := range []string{`users`, `orgs`, `memberships`, `api_keys`, `api_keys WHERE revoked_at IS NOT NULL`} {
			out = append(out, h.count(`SELECT count(*) FROM `+q))
		}
		return out
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			h := newLoginHub(t, newIdentity("alice", "a@x"))
			h.seedKey(h.seedUser("carol").ID, 90)
			tc.prior(h)
			before := counts(h)

			r := h.callback(loginBody)

			if r.status != 409 || r.errorField("error_type") != "conflict" {
				t.Errorf("answer = %d %s, want 409 conflict", r.status, r.body)
			}
			if after := counts(h); !slices.Equal(after, before) {
				t.Errorf("users, orgs, memberships, keys, revoked keys = %v, want %v", after, before)
			}
		})
	}
}
