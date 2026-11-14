package secret

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
)

// newKey returns a random 32-byte key.
func newKey(t *testing.T) Key {
	t.Helper()
	k := make(Key, 32)
	if _, err := rand.Read(k); err != nil {
		t.Fatal(err)
	}
	return k
}

// b64 is the standard padded base64 form an operator configures a key in.
func b64(k []byte) string { return base64.StdEncoding.EncodeToString(k) }

// writeFile writes content to a file in a fresh directory and returns its path.
func writeFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "key")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestLoadKeys_TakesTheConfiguredKeys(t *testing.T) {
	envKey, fileKey, prevKey := newKey(t), newKey(t), newKey(t)
	missing := filepath.Join(t.TempDir(), "missing")
	cases := []struct {
		name          string
		env           map[string]string
		keyFile       string
		current, prev Key
	}{
		{"environment only", map[string]string{"PARLEY_SECRETS_KEY": " " + b64(envKey) + "\n"}, "", envKey, nil},
		{"key file only", nil, writeFile(t, b64(fileKey)), fileKey, nil},
		{"environment wins over a readable file", map[string]string{"PARLEY_SECRETS_KEY": b64(envKey)}, writeFile(t, b64(fileKey)), envKey, nil},
		{"environment set and the file unreadable", map[string]string{"PARLEY_SECRETS_KEY": b64(envKey)}, missing, envKey, nil},
		{"key file with a trailing newline", nil, writeFile(t, b64(fileKey)+"\n"), fileKey, nil},
		{"empty environment variable counts as unset", map[string]string{"PARLEY_SECRETS_KEY": ""}, writeFile(t, b64(fileKey)), fileKey, nil},
		{"previous key", map[string]string{"PARLEY_SECRETS_KEY": b64(envKey), "PARLEY_SECRETS_KEY_PREVIOUS": b64(prevKey)}, "", envKey, prevKey},
		{"nothing set", nil, "", nil, nil},
		{"previous without current", map[string]string{"PARLEY_SECRETS_KEY_PREVIOUS": b64(prevKey)}, "", nil, prevKey},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			keys, err := LoadKeys(config.MapEnv(tc.env), tc.keyFile)
			if err != nil {
				t.Fatalf("LoadKeys: %v", err)
			}
			if !bytes.Equal(keys.Current, tc.current) || (keys.Current == nil) != (tc.current == nil) {
				t.Errorf("current key = %x, want %x", []byte(keys.Current), []byte(tc.current))
			}
			if !bytes.Equal(keys.Previous, tc.prev) || (keys.Previous == nil) != (tc.prev == nil) {
				t.Errorf("previous key = %x, want %x", []byte(keys.Previous), []byte(tc.prev))
			}
		})
	}
}

func TestLoadKeys_RefusesAMalformedKeyWithoutQuotingIt(t *testing.T) {
	good := b64(make([]byte, 32))
	allOnes := bytes.Repeat([]byte{0xff}, 32)
	cases := []struct {
		name, variable, value, source string
		inFile                        bool
	}{
		{"31 bytes", "PARLEY_SECRETS_KEY", b64(make([]byte, 31)), "PARLEY_SECRETS_KEY", false},
		{"33 bytes", "PARLEY_SECRETS_KEY", b64(make([]byte, 33)), "PARLEY_SECRETS_KEY", false},
		{"not base64", "PARLEY_SECRETS_KEY", "not-a-key-at-all", "PARLEY_SECRETS_KEY", false},
		{"unpadded", "PARLEY_SECRETS_KEY", strings.TrimRight(good, "="), "PARLEY_SECRETS_KEY", false},
		{"URL-safe alphabet", "PARLEY_SECRETS_KEY", base64.URLEncoding.EncodeToString(allOnes), "PARLEY_SECRETS_KEY", false},
		{"malformed previous key", "PARLEY_SECRETS_KEY_PREVIOUS", "hunter2hunter2", "PARLEY_SECRETS_KEY_PREVIOUS", false},
		{"key file missing", "", "", "key_file", false},
		{"key file holding 16 bytes", "", b64(make([]byte, 16)), "key_file", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := map[string]string{}
			keyFile := ""
			switch {
			case tc.variable != "":
				env[tc.variable] = tc.value
			case tc.inFile:
				keyFile = writeFile(t, tc.value)
			default:
				keyFile = filepath.Join(t.TempDir(), "missing")
			}
			_, err := LoadKeys(config.MapEnv(env), keyFile)
			if err == nil {
				t.Fatal("LoadKeys accepted the key")
			}
			if !strings.Contains(err.Error(), tc.source) {
				t.Errorf("error %q does not name %s", err, tc.source)
			}
			if tc.source == "key_file" && !strings.Contains(err.Error(), keyFile) {
				t.Errorf("error %q does not name the file %s", err, keyFile)
			}
			if tc.value != "" && strings.Contains(err.Error(), tc.value) {
				t.Errorf("error %q quotes the configured value", err)
			}
		})
	}
}

func TestKey_PrintsNoKeyMaterial(t *testing.T) {
	k := newKey(t)
	keys := Keys{Current: k, Previous: k}
	var logged bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logged, nil))
	logger.Info("x", "keys", keys, "key", k, "partial", Keys{Current: k})
	marshalled, err := json.Marshal(struct {
		Keys Keys
		Key  Key
	}{keys, k})
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	for _, s := range []string{fmt.Sprintf("%v %+v %s %x %q", keys, keys, k, k, k), fmt.Sprint(keys), logged.String(), string(marshalled)} {
		if strings.Contains(s, b64(k)) || strings.Contains(s, hex.EncodeToString(k)) || strings.Contains(s, string(k)) {
			t.Errorf("printed keys %q carry the key", s)
		}
	}
	if want := `"partial":{"Current":"[key]","Previous":null}`; !strings.Contains(logged.String(), want) {
		t.Errorf("logged %s, want it to hold %s", logged.String(), want)
	}
}

func TestKeyID_IsTheStartOfTheKeysSHA256(t *testing.T) {
	k := newKey(t)
	sum := sha256.Sum256(k)
	if got, want := KeyID(k), hex.EncodeToString(sum[:8]); got != want {
		t.Errorf("KeyID = %q, want %q", got, want)
	}
}
