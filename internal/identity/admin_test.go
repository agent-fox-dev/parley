package identity_test

import (
	"context"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/store"
)

// openStore opens a new store with the admin settings' schema step.
func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{
		DatabasePath:  filepath.Join(dir, "parley.db"),
		WorkspaceRoot: filepath.Join(dir, "workspaces"),
	}, []store.Step{identity.AdminSettingsStep})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestGenerateAdminToken_ReturnsADifferentTokenOfTheHubsFormatEachTime(t *testing.T) {
	t.Parallel()
	format := regexp.MustCompile(`^parley_admin_[0-9a-f]{64}$`)
	seen := map[string]bool{}
	for range 200 {
		tok, err := identity.GenerateAdminToken()
		if err != nil {
			t.Fatalf("GenerateAdminToken: %v", err)
		}
		if !format.MatchString(tok) {
			t.Fatalf("token %q does not match %s", tok, format)
		}
		if seen[tok] {
			t.Fatalf("token %q generated twice", tok)
		}
		seen[tok] = true
	}
}

func TestAdminSettings_ReportsNoneOnANewHub(t *testing.T) {
	t.Parallel()
	st := openStore(t)
	got, err := identity.AdminSettings(context.Background(), st)
	if err != nil {
		t.Fatalf("AdminSettings: %v", err)
	}
	if got != (identity.Admin{}) || got.Stored() {
		t.Errorf("AdminSettings on a new hub = %+v (stored %v), want none", got, got.Stored())
	}
}

func TestIssueAdminToken_StoresTheEmailOnlyWithTheFirstToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	st := openStore(t)
	var first, second string
	if err := identity.IssueAdminToken(ctx, st, "a@x", func(tok string) error { first = tok; return nil }); err != nil {
		t.Fatalf("first IssueAdminToken: %v", err)
	}
	if err := identity.IssueAdminToken(ctx, st, "b@x", func(tok string) error { second = tok; return nil }); err != nil {
		t.Fatalf("second IssueAdminToken: %v", err)
	}
	got, err := identity.AdminSettings(ctx, st)
	if err != nil {
		t.Fatalf("AdminSettings: %v", err)
	}
	want := identity.Admin{Email: "a@x", TokenHash: identity.HashAdminToken(second)}
	if got != want {
		t.Errorf("AdminSettings = %+v, want %+v", got, want)
	}
	if first == second {
		t.Errorf("both issues delivered %q", first)
	}
	var rows int
	if err := st.Main().QueryRowContext(ctx, `SELECT count(*) FROM admin_settings`).Scan(&rows); err != nil {
		t.Fatalf("count admin settings: %v", err)
	}
	if rows != 1 {
		t.Errorf("admin_settings holds %d rows, want 1", rows)
	}
}

func TestVerifyAdminToken_AcceptsOnlyTheStoredToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	empty := openStore(t)
	bootstrapped := openStore(t)
	var tok string
	if err := identity.IssueAdminToken(ctx, bootstrapped, "a@x", func(issued string) error { tok = issued; return nil }); err != nil {
		t.Fatalf("IssueAdminToken: %v", err)
	}
	other, err := identity.GenerateAdminToken()
	if err != nil {
		t.Fatalf("GenerateAdminToken: %v", err)
	}
	for _, tc := range []struct {
		name      string
		st        *store.Store
		presented string
		want      bool
	}{
		{"no token stored", empty, tok, false},
		{"no token stored, empty token", empty, "", false},
		{"the stored token", bootstrapped, tok, true},
		{"another token", bootstrapped, other, false},
		{"the stored token's hash", bootstrapped, identity.HashAdminToken(tok), false},
		{"empty token", bootstrapped, "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, err := identity.VerifyAdminToken(ctx, tc.st, tc.presented)
			if err != nil {
				t.Fatalf("VerifyAdminToken: %v", err)
			}
			if got != tc.want {
				t.Errorf("VerifyAdminToken = %v, want %v", got, tc.want)
			}
		})
	}
}
