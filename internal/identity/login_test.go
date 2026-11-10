package identity

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"testing"
	"time"

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

// loggedIn has alice from an earlier login with github id 123.
type loggedIn struct {
	*testHub
	reader *stubReader
	alice  map[string]any
	key    string
}

func newLoggedIn(t *testing.T) *loggedIn {
	t.Helper()
	l := &loggedIn{testHub: newTestHub(t), reader: &stubReader{id: Identity{Provider: "github", ProviderID: "123", Username: "alice", FullName: "Alice A", Email: "a@x"}}}
	l.withRoutes(l.reader, github)
	r := l.callback(loginBody)
	if r.status != 200 {
		t.Fatalf("first login: %d %s", r.status, r.body)
	}
	l.alice, _ = r.doc["user"].(map[string]any)
	key, _ := r.doc["api_key"].(map[string]any)
	l.key = key["key"].(string)
	return l
}

func (l *loggedIn) storedAlice() (username, email string, updated int64) {
	l.t.Helper()
	if err := l.st.DB().QueryRow(`SELECT username, email, updated_at FROM users WHERE id = ?`, l.alice["id"]).Scan(&username, &email, &updated); err != nil {
		l.t.Fatal(err)
	}
	return
}

func TestCallback_ALaterLoginRefreshesTheUser(t *testing.T) {
	l := newLoggedIn(t)
	l.exec(`UPDATE users SET role = 'admin', full_name = 'Custom' WHERE id = ?`, l.alice["id"])
	l.clock.set(t0.Add(time.Hour))
	l.reader.id = Identity{Provider: "github", ProviderID: "123", Username: "alicia", FullName: "Other", Email: "new@x"}
	r := l.callback(loginBody)
	u, _ := r.doc["user"].(map[string]any)
	if r.status != 200 || u["id"] != l.alice["id"] || u["username"] != "alicia" || u["email"] != "new@x" || u["role"] != "admin" ||
		u["full_name"] != "Custom" || u["created_at"] != l.alice["created_at"] || u["updated_at"] != "2026-01-02T04:04:05Z" {
		t.Fatalf("%d %v", r.status, u)
	}
	if c := l.counts(); c[0] != 1 || c[1] != 1 || c[2] != 1 {
		t.Errorf("counts %v", c)
	}
	var name, slug string
	l.st.DB().QueryRow(`SELECT name, slug FROM orgs`).Scan(&name, &slug)
	if name != "alice" || slug != "alice" {
		t.Errorf("org %s %s", name, slug)
	}
}

func TestCallback_ARefreshedUsernameThatIsTakenKeepsTheOldOne(t *testing.T) {
	l := newLoggedIn(t)
	l.seedUser("bob")
	l.reader.id = Identity{Provider: "github", ProviderID: "123", Username: "bob", Email: "new@x"}
	r := l.callback(loginBody)
	u, _ := r.doc["user"].(map[string]any)
	if r.status != 200 || u["username"] != "alice" || u["email"] != "new@x" {
		t.Fatalf("%d %v", r.status, u)
	}
	var bob string
	l.st.DB().QueryRow(`SELECT username FROM users WHERE email = 'bob@x'`).Scan(&bob)
	if bob != "bob" {
		t.Errorf("bob became %q", bob)
	}
}

func TestCallback_ABlockedUserIsRefusedAndNothingChanges(t *testing.T) {
	l := newLoggedIn(t)
	l.exec(`UPDATE users SET status = 'blocked' WHERE id = ?`, l.alice["id"])
	_, _, updatedBefore := l.storedAlice()
	before := l.counts()
	l.reader.id = Identity{Provider: "github", ProviderID: "123", Username: "alicia", Email: "new@x"}
	r := l.callback(loginBody)
	if r.status != 403 || r.errorMember("error_type") != "forbidden" || r.errorMember("message") != "user is blocked" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if name, email, updated := l.storedAlice(); name != "alice" || email != "a@x" || updated != updatedBefore {
		t.Errorf("alice changed: %s %s %d", name, email, updated)
	}
	if c := l.counts(); c != before {
		t.Errorf("counts %v, before %v", c, before)
	}
}

func TestCallback_EveryLoginReplacesTheUsersKeys(t *testing.T) {
	type row struct {
		expires string
		days    int
	}
	for _, tt := range []row{{"", 90}, {`,"expires":90`, 90}, {`,"expires":60`, 60}, {`,"expires":30`, 30}, {`,"expires":0`, 0}} {
		l := newLoggedIn(t)
		alice := l.alice["id"].(string)
		k1 := l.key
		k2, _ := l.issueKey(alice)
		_, k0ID := l.issueKey(alice)
		dayAgo := t0.Add(-24 * time.Hour)
		l.exec(`UPDATE api_keys SET revoked_at = ? WHERE key_id = ?`, nanos(dayAgo), k0ID)
		pat, _ := l.storePAT(alice, PATSpec{Name: "ci", Permissions: []string{"git:read"}})
		bob := l.seedUser("bob")
		bobKey, _ := l.issueKey(bob.ID)
		l.clock.set(t0.Add(time.Hour))
		body := `{"provider":"github","code":"c","redirect_uri":"http://127.0.0.1:5000/callback"` + tt.expires + `}`
		r := l.callback(body)
		if r.status != 200 {
			t.Fatalf("%s: %d %s", tt.expires, r.status, r.body)
		}
		key, _ := r.doc["api_key"].(map[string]any)
		want := any(nil)
		if tt.days > 0 {
			want = t0.Add(time.Hour).Add(time.Duration(tt.days) * 24 * time.Hour).Format("2006-01-02T15:04:05Z")
		}
		if key["expires_at"] != want {
			t.Errorf("%s: expires_at %v, want %v", tt.expires, key["expires_at"], want)
		}
		if tt.expires != "" {
			continue
		}
		for _, old := range []string{k1, k2} {
			if m := l.get("/api/v1/test/whoami", bearer(old)).errorMember("message"); m != "credential revoked" {
				t.Errorf("an old key: %v", m)
			}
		}
		var k0Revoked int64
		l.st.DB().QueryRow(`SELECT revoked_at FROM api_keys WHERE key_id = ?`, k0ID).Scan(&k0Revoked)
		if k0Revoked != nanos(dayAgo) {
			t.Error("an already revoked key got a new revoked_at")
		}
		for _, cred := range []string{pat, bobKey, key["key"].(string)} {
			if r := l.get("/api/v1/test/whoami", bearer(cred)); r.status != 200 {
				t.Errorf("%s: %d", cred[:12], r.status)
			}
		}
		var live int
		l.st.DB().QueryRow(`SELECT count(*) FROM api_keys WHERE user_id = ? AND revoked_at IS NULL`, alice).Scan(&live)
		if live != 1 {
			t.Errorf("alice holds %d live keys", live)
		}
	}
}

func TestCallback_ARefreshedEmailOfAnotherUserIsAConflict(t *testing.T) {
	l := newLoggedIn(t)
	l.seedUser("b")
	l.exec(`UPDATE users SET email = 'b@x' WHERE username = 'b'`)
	before := l.counts()
	l.reader.id = Identity{Provider: "github", ProviderID: "123", Username: "alicia", Email: "b@x"}
	r := l.callback(loginBody)
	if r.status != 409 || r.errorMember("error_type") != "conflict" {
		t.Fatalf("%d %s", r.status, r.body)
	}
	if name, email, _ := l.storedAlice(); name != "alice" || email != "a@x" {
		t.Errorf("alice changed: %s %s", name, email)
	}
	if c := l.counts(); c != before {
		t.Errorf("counts %v, before %v", c, before)
	}
	if r := l.get("/api/v1/test/whoami", bearer(l.key)); r.status != 200 {
		t.Errorf("alice's key: %d", r.status)
	}
}
