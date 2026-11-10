package identity

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
)

// stubReader returns the identity, or the error, the test sets.
type stubReader struct {
	id    Identity
	err   error
	calls int
}

func (s *stubReader) Identity(context.Context, string, string, string) (Identity, error) {
	s.calls++
	return s.id, s.err
}

var github = config.Provider{Name: "github", ClientID: "gh-id", ClientSecret: "gh-secret"}

const loginBody = `{"provider":"github","code":"c","redirect_uri":"http://127.0.0.1:5000/callback"}`

func (h *testHub) callback(body string) hubResponse {
	h.t.Helper()
	return h.do("POST", "/api/v1/auth/callback", "", body)
}

// counts are the numbers of users, orgs, memberships, keys and revoked keys.
func (h *testHub) counts() [5]int {
	h.t.Helper()
	var c [5]int
	for i, q := range []string{
		`SELECT count(*) FROM users`, `SELECT count(*) FROM orgs`, `SELECT count(*) FROM memberships`,
		`SELECT count(*) FROM api_keys`, `SELECT count(*) FROM api_keys WHERE revoked_at IS NOT NULL`,
	} {
		if err := h.st.DB().QueryRow(q).Scan(&c[i]); err != nil {
			h.t.Fatal(err)
		}
	}
	return c
}

var keyForm = regexp.MustCompile(`^parley_([0-9A-Za-z]{8})_[0-9A-Za-z]{32}$`)

func TestCallback_AFirstLoginCreatesTheUserTheOrgAndAKey(t *testing.T) {
	h := newTestHub(t)
	h.withRoutes(&stubReader{id: Identity{Provider: "github", ProviderID: "123", Username: "alice", FullName: "Alice A", Email: "a@x"}}, github)
	r := h.callback(loginBody)
	if r.status != 200 {
		t.Fatalf("%d %s", r.status, r.body)
	}
	u, _ := r.doc["user"].(map[string]any)
	want := map[string]any{"username": "alice", "email": "a@x", "full_name": "Alice A", "status": "active", "provider": "github",
		"provider_id": "123", "role": "user", "created_at": "2026-01-02T03:04:05Z", "updated_at": "2026-01-02T03:04:05Z"}
	for k, v := range want {
		if u[k] != v {
			t.Errorf("user.%s = %v, want %v", k, u[k], v)
		}
	}
	key, _ := r.doc["api_key"].(map[string]any)
	if len(key) != 3 || key["expires_at"] == nil {
		t.Errorf("api_key = %v", key)
	}
	m := keyForm.FindStringSubmatch(fmt.Sprint(key["key"]))
	if m == nil || m[1] != key["key_id"] {
		t.Errorf("key %v does not match its key_id %v", key["key"], key["key_id"])
	}
	var name, slug, owner, status string
	if err := h.st.DB().QueryRow(`SELECT name, slug, owner_id, status FROM orgs`).Scan(&name, &slug, &owner, &status); err != nil {
		t.Fatal(err)
	}
	if name != "alice" || slug != "alice" || owner != u["id"] || status != "active" {
		t.Errorf("org = %s %s %s %s", name, slug, owner, status)
	}
	if c := h.counts(); c != [5]int{1, 1, 1, 1, 0} {
		t.Errorf("counts = %v", c)
	}
	w := h.get("/api/v1/test/whoami", bearer(fmt.Sprint(key["key"])))
	if user, _ := w.doc["user"].(map[string]any); w.status != 200 || user["id"] != u["id"] {
		t.Errorf("the new key: %d %s", w.status, w.body)
	}
}

func TestCallback_ATakenUsernameGetsTheFirstFreeSuffix(t *testing.T) {
	tests := []struct {
		existing int // alice plus alice-2 … alice-<existing>
		want     string
	}{{1, "alice-2"}, {3, "alice-4"}, {9, "alice-10"}}
	for _, tt := range tests {
		h := newTestHub(t)
		h.seedUser("alice")
		for i := 2; i <= tt.existing; i++ {
			h.seedUser(fmt.Sprint("alice-", i))
		}
		h.withRoutes(&stubReader{id: Identity{Provider: "github", ProviderID: "777", Username: "alice", Email: "new@x"}}, github)
		r := h.callback(loginBody)
		u, _ := r.doc["user"].(map[string]any)
		if r.status != 200 || u["username"] != tt.want {
			t.Errorf("existing %d: %d %s", tt.existing, r.status, r.body)
			continue
		}
		var name string
		h.st.DB().QueryRow(`SELECT name FROM orgs WHERE owner_id = ?`, u["id"]).Scan(&name)
		if name != tt.want {
			t.Errorf("org name %q, want %q", name, tt.want)
		}
	}
}

func TestCallback_TheBootstrapAdminEmailMakesTheFirstAdmin(t *testing.T) {
	tests := []struct {
		name       string
		adminEmail string
		email      string
		existing   string // role and status of an existing user, or ""
		want       string
	}{
		{"matching, no admin yet", "Boss@X.io", "boss@x.io", "", "admin"},
		{"matching, an admin exists", "Boss@X.io", "boss@x.io", "admin/active", "user"},
		{"matching, a blocked admin exists", "Boss@X.io", "boss@x.io", "admin/blocked", "user"},
		{"another e-mail", "boss@x.io", "a@x", "", "user"},
		{"no admin e-mail", "", "boss@x.io", "", "user"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHub(t)
			if tt.adminEmail != "" {
				h.tx(func(tx *sql.Tx) error { return SetAdmin(context.Background(), tx, tt.adminEmail, HashToken("t")) })
			}
			if tt.existing != "" {
				o := h.seedUser("old")
				role, status := tt.existing[:len("admin")], tt.existing[len("admin/"):]
				h.exec(`UPDATE users SET role = ?, status = ? WHERE id = ?`, role, status, o.ID)
			}
			h.withRoutes(&stubReader{id: Identity{Provider: "github", ProviderID: "5", Username: "boss", Email: tt.email}}, github)
			r := h.callback(loginBody)
			u, _ := r.doc["user"].(map[string]any)
			if r.status != 200 || u["role"] != tt.want {
				t.Fatalf("%d %s, want role %s", r.status, r.body, tt.want)
			}
			var stored string
			h.st.DB().QueryRow(`SELECT role FROM users WHERE id = ?`, u["id"]).Scan(&stored)
			if stored != tt.want {
				t.Errorf("stored role %q", stored)
			}
		})
	}
}

func TestCallback_AUniquenessConflictCreatesNothing(t *testing.T) {
	tests := []struct {
		name    string
		arrange func(h *testHub)
	}{
		{"e-mail of another user", func(h *testHub) {
			b := h.seedUser("bob")
			h.exec(`UPDATE users SET email = 'a@x' WHERE id = ?`, b.ID)
		}},
		{"an org named after the username", func(h *testHub) { h.seedOrg("alice", "other") }},
		{"every username taken", func(h *testHub) {
			h.seedUser("alice")
			for i := 2; i <= 10; i++ {
				h.seedUser(fmt.Sprint("alice-", i))
			}
		}},
		{"every slug taken", func(h *testHub) {
			h.seedOrg("o0", "alice")
			for i := 1; i <= 10; i++ {
				h.seedOrg(fmt.Sprint("o", i), fmt.Sprint("alice-", i))
			}
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := newTestHub(t)
			tt.arrange(h)
			before := h.counts()
			h.withRoutes(&stubReader{id: Identity{Provider: "github", ProviderID: "123", Username: "alice", Email: "a@x"}}, github)
			r := h.callback(loginBody)
			if r.status != 409 || r.errorMember("error_type") != "conflict" {
				t.Errorf("%d %s", r.status, r.body)
			}
			if c := h.counts(); c != before {
				t.Errorf("counts %v, before %v", c, before)
			}
		})
	}
}
