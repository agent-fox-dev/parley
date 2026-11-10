package identity

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	wantAPIKeyForm = regexp.MustCompile(`^parley_[0-9A-Za-z]{8}_[0-9A-Za-z]{32}$`)
	wantPATForm    = regexp.MustCompile(`^parley_pat_[a-z0-9]{8}_[a-z0-9]{32}$`)
)

func secretOf(cred string) string { return cred[strings.LastIndex(cred, "_")+1:] }

func TestCredentials_OnlyTheSecretsHashIsStored(t *testing.T) {
	h := newTestHub(t)
	alice := h.seedUser("alice")
	key, keyID := h.issueKey(alice.ID)
	key2, keyID2 := h.issueKey(alice.ID)
	pat, _ := h.storePAT(alice.ID, PATSpec{Name: "ci", Permissions: []string{"git:read"}})
	if !wantAPIKeyForm.MatchString(key) || !wantPATForm.MatchString(pat) {
		t.Fatalf("forms: %q %q", key, pat)
	}
	if keyID == keyID2 || secretOf(key) == secretOf(key2) {
		t.Error("two keys share an id or a secret")
	}
	if err := h.st.Close(); err != nil {
		t.Fatal(err)
	}
	var data bytes.Buffer
	filepath.WalkDir(filepath.Join(h.dir, "data"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			data.Write(b)
		}
		return nil
	})
	for _, cred := range []string{key, pat} {
		if bytes.Contains(data.Bytes(), []byte(secretOf(cred))) {
			t.Errorf("the secret of %s is stored", cred[:12])
		}
	}
	sum := sha256.Sum256([]byte(secretOf(key)))
	if !bytes.Contains(data.Bytes(), []byte(hex.EncodeToString(sum[:]))) {
		t.Error("the hash of the key's secret is not stored")
	}
}
