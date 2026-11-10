package identity

import (
	"context"
	"database/sql"
	"fmt"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
)

func TestBaseSlug_DerivesTheSlugFromTheUsername(t *testing.T) {
	const id = "0a1b2c3d-0000-4000-8000-000000000000"
	tests := map[string]string{
		"Alice.Smith":  "alice-smith",
		"42bob":        "u-42bob",
		"a":            "u-0a1b2c3d",
		"9":            "u-0a1b2c3d",
		"--x__":        "u-0a1b2c3d",
		"_bob_":        "bob",
		"A  B":         "a-b",
		"Alice Smith":  "alice-smith",
		"a..b--c":      "a-b-c",
		"ünï":          "u-0a1b2c3d",
		"snake_case-x": "snake_case-x",
	}
	for username, want := range tests {
		if got := baseSlug(username, id); got != want {
			t.Errorf("baseSlug(%q) = %q, want %q", username, got, want)
		}
	}
}

// seedOrg stores an org with the given name and slug.
func (h *testHub) seedOrg(name, slug string) {
	h.t.Helper()
	h.exec(`INSERT INTO orgs (id, name, slug, status, created_at, updated_at) VALUES (?, ?, ?, 'active', 0, 0)`, NewID(), name, slug)
}

func TestSignIn_ATakenSlugGetsANumberedSuffix(t *testing.T) {
	tests := []struct {
		taken []string
		want  string
	}{
		{[]string{"alice"}, "alice-1"},
		{[]string{"alice", "alice-1", "alice-2", "alice-3", "alice-4", "alice-5", "alice-6", "alice-7", "alice-8", "alice-9"}, "alice-10"},
	}
	for _, tt := range tests {
		h := newTestHub(t)
		for i, s := range tt.taken {
			h.seedOrg(fmt.Sprint("other-", i), s)
		}
		var u User
		h.tx(func(tx *sql.Tx) error {
			var err error
			u, err = CreateUserWithOrg(context.Background(), tx, NewUser{Username: "alice", Email: "a@x"}, RoleUser, h.clock.Now())
			return err
		})
		var slug string
		h.st.DB().QueryRow(`SELECT slug FROM orgs WHERE owner_id = ?`, u.ID).Scan(&slug)
		if slug != tt.want {
			t.Errorf("taken %v: slug %q, want %q", tt.taken, slug, tt.want)
		}
	}
	h := newTestHub(t)
	h.withRoutes(&stubReader{id: Identity{Provider: "github", ProviderID: "9", Username: "Alice.Smith", Email: "as@x"}}, config.Provider{Name: "github", ClientID: "i", ClientSecret: "s"})
	if r := h.callback(`{"provider":"github","code":"c","redirect_uri":"http://127.0.0.1:5000/callback"}`); r.status != 200 {
		t.Fatalf("login: %d %s", r.status, r.body)
	}
	var slug string
	h.st.DB().QueryRow(`SELECT slug FROM orgs`).Scan(&slug)
	if slug != "alice-smith" {
		t.Errorf("slug of the login's org = %q", slug)
	}
}
