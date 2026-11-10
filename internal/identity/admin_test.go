package identity

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/agent-fox-dev/parley/internal/store"
)

var tokenForm = regexp.MustCompile(`^parley_admin_[0-9a-f]{64}$`)

func TestGenerateAdminToken_IsAFreshTokenOfTheAdminForm(t *testing.T) {
	seen := map[string]bool{}
	for range 100 {
		tok, err := GenerateAdminToken()
		if err != nil {
			t.Fatal(err)
		}
		if !tokenForm.MatchString(tok) {
			t.Fatalf("token %q is not of the admin form", tok)
		}
		if seen[tok] {
			t.Fatal("a token was generated twice")
		}
		seen[tok] = true
	}
}

func TestHashToken_IsTheLowercaseHexSHA256OfTheToken(t *testing.T) {
	tok := "parley_admin_" + hex.EncodeToString(make([]byte, 32))
	sum := sha256.Sum256([]byte(tok))
	if got := HashToken(tok); got != hex.EncodeToString(sum[:]) {
		t.Errorf("HashToken = %q", got)
	}
}

func openStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{DatabasePath: filepath.Join(dir, "p.db"), WorkspaceRoot: filepath.Join(dir, "ws")}, []store.Step{AdminSettingsStep})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

func TestAdminSettings_ReportsWhatIsStored(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	s, err := AdminSettings(ctx, st)
	if err != nil || s.Stored || s.Email != "" || s.TokenHash != "" {
		t.Fatalf("new hub: %+v, %v", s, err)
	}
	tok, _ := GenerateAdminToken()
	if err := st.Tx(ctx, func(tx *sql.Tx) error { return SetAdmin(ctx, tx, "a@x", HashToken(tok)) }); err != nil {
		t.Fatal(err)
	}
	s, err = AdminSettings(ctx, st)
	if err != nil || !s.Stored || s.Email != "a@x" || s.TokenHash != HashToken(tok) {
		t.Fatalf("after SetAdmin: %+v, %v", s, err)
	}
	next, _ := GenerateAdminToken()
	if err := st.Tx(ctx, func(tx *sql.Tx) error { return SetAdmin(ctx, tx, "", HashToken(next)) }); err != nil {
		t.Fatal(err)
	}
	if s, _ := AdminSettings(ctx, st); s.Email != "a@x" || s.TokenHash != HashToken(next) {
		t.Errorf("a reset without an e-mail = %+v, want the e-mail kept and the new hash", s)
	}
}

func TestVerifyAdminToken_AcceptsOnlyTheStoredToken(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	tok, _ := GenerateAdminToken()
	if ok, err := VerifyAdminToken(ctx, st, tok); ok || err != nil {
		t.Errorf("nothing stored: %v, %v; want false", ok, err)
	}
	if err := st.Tx(ctx, func(tx *sql.Tx) error { return SetAdmin(ctx, tx, "a@x", HashToken(tok)) }); err != nil {
		t.Fatal(err)
	}
	other, _ := GenerateAdminToken()
	for _, tt := range []struct {
		presented string
		want      bool
	}{{tok, true}, {other, false}, {"", false}} {
		if ok, err := VerifyAdminToken(ctx, st, tt.presented); ok != tt.want || err != nil {
			t.Errorf("Verify(%q) = %v, %v; want %v", tt.presented, ok, err, tt.want)
		}
	}
}
