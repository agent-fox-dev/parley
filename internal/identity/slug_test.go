package identity

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/store"
)

func TestSlug_DerivesThePersonalOrgSlugFromTheUsername(t *testing.T) {
	t.Parallel()
	const id = "0a1b2c3d-4e5f-4a6b-8c7d-9e0f1a2b3c4d"
	cases := map[string]string{
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
	for username, want := range cases {
		if got := slug(username, id); got != want {
			t.Errorf("slug(%q) = %q, want %q", username, got, want)
		}
	}
}

func TestCreateUserAndPersonalOrg_TriesTheSlugSuffixesInOrder(t *testing.T) {
	t.Parallel()
	cases := []struct {
		taken int // the slugs alice, alice-1 … alice-<taken-1> are taken
		want  string
	}{
		{1, "alice-1"},
		{10, "alice-10"},
	}
	for _, tc := range cases {
		t.Run(tc.want, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			st, err := store.Open(ctx, store.Options{DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "ws")},
				[]store.Step{SchemaStep})
			if err != nil {
				t.Fatalf("open store: %v", err)
			}
			t.Cleanup(func() { st.Close() })
			var org Org
			err = st.Tx(ctx, func(tx *sql.Tx) error {
				for i := range tc.taken {
					s := "alice"
					if i > 0 {
						s = fmt.Sprintf("alice-%d", i)
					}
					_, err := tx.ExecContext(ctx, `INSERT INTO orgs (id, name, slug, status, created_at, updated_at)
VALUES (?, ?, ?, 'active', 0, 0)`, s, "org "+s, s)
					if err != nil {
						return err
					}
				}
				nu := NewUser{Username: "alice", Email: "a@x", Provider: "github", ProviderID: "1", CreatedAt: time.Now()}
				_, org, err = CreateUserAndPersonalOrg(ctx, tx, nu, RoleUser)
				return err
			})
			if err != nil || org.Slug != tc.want || org.Name != "alice" {
				t.Errorf("org = name %q, slug %q (%v); want alice, %s", org.Name, org.Slug, err, tc.want)
			}
		})
	}
}
