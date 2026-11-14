package secret_test

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
)

const noKeyWarning = "secrets cannot be stored: no encryption key configured"

// newKey returns a random key in the raw form and in the base64 form an
// operator configures it in.
func newKey(t *testing.T) (raw []byte, encoded string) {
	t.Helper()
	raw = make([]byte, 32)
	if _, err := rand.Read(raw); err != nil {
		t.Fatal(err)
	}
	return raw, base64.StdEncoding.EncodeToString(raw)
}

// logLines decodes every line of a hub's log whose msg is msg.
func logLines(t *testing.T, log, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for line := range strings.SplitSeq(log, "\n") {
		var rec map[string]any
		if json.Unmarshal([]byte(line), &rec) == nil && rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

func TestStart_StopsBeforeListeningOnAKeyThatCannotBeLoaded(t *testing.T) {
	cases := []struct {
		name, toml string
		env        map[string]string
		names      string
		value      string
	}{
		{"malformed current key", "", map[string]string{secret.EnvCurrentKey: "not-a-key"}, secret.EnvCurrentKey, "not-a-key"},
		{"malformed previous key", "", map[string]string{secret.EnvPreviousKey: "hunter2hunter2"}, secret.EnvPreviousKey, "hunter2hunter2"},
		{"unreadable key file", "[secrets]\nkey_file = \"missing.key\"\n", nil, "missing.key", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.toml"),
				[]byte("[server]\nbind = \"127.0.0.1\"\nport = 0\n"+tc.toml), 0o600); err != nil {
				t.Fatal(err)
			}
			env := map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir}
			for k, v := range tc.env {
				env[k] = v
			}
			var stderr hubtest.LogBuffer
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			code := hub.Run(ctx, hub.Options{Env: config.MapEnv(env), WorkDir: dir, Stderr: &stderr})
			if code != 1 {
				t.Fatalf("Run = %d, want 1; stderr = %s", code, stderr.String())
			}
			log := strings.TrimSpace(stderr.String())
			if lines := strings.Split(log, "\n"); len(lines) != 1 {
				t.Fatalf("stderr holds %d lines, want 1: %s", len(lines), log)
			}
			errs := logLines(t, log, "cannot start")
			if len(errs) != 1 || errs[0]["level"] != "ERROR" {
				t.Fatalf("stderr %s is not one cannot start error line", log)
			}
			text, _ := errs[0]["error"].(string)
			if !strings.Contains(text, tc.names) {
				t.Errorf("error %q does not name %s", text, tc.names)
			}
			if tc.value != "" && strings.Contains(log, tc.value) {
				t.Errorf("stderr %s quotes the configured value", log)
			}
			if len(logLines(t, log, "listening")) != 0 {
				t.Errorf("the hub listened: %s", log)
			}
		})
	}
}

func TestStart_ReportsSecretEncryptionAndStartsWithoutAKey(t *testing.T) {
	_, current := newKey(t)
	_, previous := newKey(t)
	cases := []struct {
		name                string
		cfg                 hubtest.Config
		configured, hasPrev bool
	}{
		{"no key", hubtest.Config{}, false, false},
		{"current and previous key from the environment",
			hubtest.Config{Env: map[string]string{secret.EnvCurrentKey: current, secret.EnvPreviousKey: previous}}, true, true},
		{"current key from a key file relative to the config dir",
			hubtest.Config{TOML: "[secrets]\nkey_file = \"keys/current\"\n", Files: map[string]string{"keys/current": current + "\n"}}, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hubtest.StartWith(t, tc.cfg)
			log := h.Log.String()
			reports := logLines(t, log, "secret encryption")
			if len(reports) != 1 {
				t.Fatalf("%d secret encryption lines, want 1; log = %s", len(reports), log)
			}
			want := map[string]any{"level": "INFO", "key_configured": tc.configured, "previous_key_present": tc.hasPrev, "undecryptable_secrets": float64(0)}
			for k, v := range want {
				if reports[0][k] != v {
					t.Errorf("%s = %v, want %v", k, reports[0][k], v)
				}
			}
			warnings := logLines(t, log, noKeyWarning)
			if want := map[bool]int{false: 1}[tc.configured]; len(warnings) != want {
				t.Errorf("%d no-key warnings, want %d; log = %s", len(warnings), want, log)
			}
			for _, w := range warnings {
				if w["level"] != "WARN" {
					t.Errorf("the no-key warning is at %v, want WARN", w["level"])
				}
			}
			if status, body := h.Call(t, http.MethodGet, "/readyz", "", nil); status != http.StatusOK || !strings.Contains(string(body), `"ready"`) {
				t.Errorf("GET /readyz = %d %s, want 200 ready", status, body)
			}
		})
	}
}

func TestStart_KeepsTheKeysOutOfTheLogAndTheStore(t *testing.T) {
	currentRaw, current := newKey(t)
	previousRaw, previous := newKey(t)
	passEnded := make(chan struct{})
	h := hubtest.StartWith(t, hubtest.Config{
		TOML: "[logging]\nlevel = \"trace\"\n",
		Env:  map[string]string{secret.EnvCurrentKey: current, secret.EnvPreviousKey: previous},
		Compose: func(c *hub.Composition) {
			c.AddStartHook("test-seed-secret", func(ctx context.Context, rt *hub.Runtime) error {
				return rt.Store.Tx(ctx, func(tx *sql.Tx) error {
					return c.Secrets().Put(ctx, tx, secret.ScopeUser, "u1", "GIT_PAT", "ghp_value")
				})
			})
			// Tasks registered after the hub's own run last in a pass, so
			// this one marks the end of the first pass.
			c.AddMaintenanceTask("test-pass-end", func(context.Context, *hub.Runtime) error {
				select {
				case <-passEnded:
				default:
					close(passEnded)
				}
				return nil
			})
		},
	})
	select {
	case <-passEnded:
	case <-time.After(5 * time.Second):
		t.Fatal("the first maintenance pass did not end within 5 s")
	}
	dataDir, _ := h.Listening["data_dir"].(string)
	h.Stop()
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0; log = %s", code, h.Log.String())
	}

	sources := map[string][]byte{"the log": []byte(h.Log.String())}
	err := filepath.WalkDir(dataDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		sources[path] = content
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) < 2 {
		t.Fatalf("no file in the data directory %s", dataDir)
	}
	forms := map[string][]byte{
		"the current key in base64": []byte(current), "the current key's bytes": currentRaw,
		"the previous key in base64": []byte(previous), "the previous key's bytes": previousRaw,
	}
	for where, content := range sources {
		for form, key := range forms {
			if bytes.Contains(content, key) {
				t.Errorf("%s holds %s", where, form)
			}
		}
	}
}
