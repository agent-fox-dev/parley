package identity_test

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/agent-fox-dev/parley/internal/identity"
)

// secretOf returns the secret part of an API key or PAT: what follows its
// last underscore.
func secretOf(credential string) string {
	return credential[len(credential)-32:]
}

func TestCredentials_AreGeneratedInTheirFormatAndStoredOnlyAsAHash(t *testing.T) {
	t.Parallel()
	h := newTestHub(t)
	alice := h.seedUser("alice")
	key, keyRec := h.seedKey(alice.ID, 30)
	other, otherRec := h.seedKey(alice.ID, 0)
	pat, _ := h.seedPAT(identity.PATSpec{UserID: alice.ID, Permissions: []string{"git:read"}})

	if !regexp.MustCompile(`^parley_[0-9A-Za-z]{8}_[0-9A-Za-z]{32}$`).MatchString(key) {
		t.Errorf("API key %q is not of the API key format", key)
	}
	if !regexp.MustCompile(`^parley_pat_[a-z0-9]{8}_[a-z0-9]{32}$`).MatchString(pat) {
		t.Errorf("PAT %q is not of the PAT format", pat)
	}
	if keyRec.KeyID == otherRec.KeyID || secretOf(key) == secretOf(other) {
		t.Errorf("two issued keys %q and %q share their key_id or secret", key, other)
	}

	if err := h.st.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	var data []byte
	err := filepath.WalkDir(h.dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := os.ReadFile(path)
		data = append(data, b...)
		return err
	})
	if err != nil {
		t.Fatalf("read the data directory: %v", err)
	}
	for name, cred := range map[string]string{"API key": key, "PAT": pat} {
		if bytes.Contains(data, []byte(secretOf(cred))) {
			t.Errorf("the data directory holds the secret of the %s", name)
		}
	}
	sum := sha256.Sum256([]byte(secretOf(key)))
	if !bytes.Contains(data, []byte(hex.EncodeToString(sum[:]))) {
		t.Errorf("the data directory does not hold the hex SHA-256 of the API key's secret")
	}
}
