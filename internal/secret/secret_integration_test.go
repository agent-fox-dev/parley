package secret_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/event"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/secret"
)

const noKeyWarning = "secrets cannot be stored: no encryption key configured"

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

	sources := dataFiles(t, dataDir)
	sources["the log"] = []byte(h.Log.String())
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

// dataFiles reads every file of a stopped hub's data directory, the
// database and its -wal and -shm files among them: path to content.
func dataFiles(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	files := map[string][]byte{}
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := os.ReadFile(path)
		files[path] = content
		return err
	})
	if err != nil || len(files) == 0 {
		t.Fatalf("read the data directory %s: %d files, %v", dir, len(files), err)
	}
	return files
}

func TestSecretValues_NeverLeaveTheHub(t *testing.T) {
	sentinels := []string{"S3ntinel-7f3a9c41-value", "S3ntinel-0b6e2d58-second"}
	_, key := newKey(t)
	var secrets *secret.Component
	var events *event.Component
	h := hubtest.StartWith(t, hubtest.Config{
		TOML:    "[logging]\nlevel = \"trace\"\n",
		Env:     map[string]string{secret.EnvCurrentKey: key},
		Compose: func(c *hub.Composition) { secrets, events = c.Secrets(), c.Events() },
	})
	userID := h.CreateUser(t, "owner")
	cred := h.IssueKey(t, userID)
	ctx := context.Background()
	// sources are every text the hub gave out or kept, by where it was.
	sources := map[string][]byte{}
	call := func(method, path, idempotencyKey string, body any, want int) {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req, err := http.NewRequest(method, h.URL("/api/v1"+path), bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+cred)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		if idempotencyKey != "" {
			req.Header.Set("Idempotency-Key", idempotencyKey)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := io.ReadAll(res.Body)
		_ = res.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		if res.StatusCode != want {
			t.Errorf("%s %s = %d %s, want %d", method, path, res.StatusCode, answer, want)
		}
		sources[fmt.Sprintf("the answer %d to %s %s", len(sources), method, path)] = answer
	}
	get := func(want string) {
		t.Helper()
		v, _, err := secrets.Get(ctx, secret.ScopeUser, userID, "git_pat")
		if err != nil {
			sources["the error of Get under "+want] = []byte(err.Error())
		}
		if v != want && (want != "unreadable" || !errors.Is(err, secret.ErrUnreadable)) {
			t.Fatalf("Get = %q, %v; want %s", v, err, want)
		}
	}

	create := entries(map[string]any{"key": "GIT_PAT", "value": sentinels[0]})
	call(http.MethodPost, "/user/secrets", "create-1", create, http.StatusCreated)
	call(http.MethodPost, "/user/secrets", "create-1", create, http.StatusOK) // the replay
	call(http.MethodGet, "/user/secrets", "", nil, http.StatusOK)
	call(http.MethodPost, "/user/secrets", "", create, http.StatusConflict)
	get(sentinels[0])
	call(http.MethodPatch, "/user/secrets/GIT_PAT", "", map[string]any{"value": sentinels[1]}, http.StatusOK)
	get(sentinels[1])
	sources["the log of the first run"] = []byte(h.Log.String())
	_, other := newKey(t)
	h.RestartEnv(t, map[string]string{secret.EnvCurrentKey: other})
	get("unreadable")
	call(http.MethodDelete, "/user/secrets/GIT_PAT", "", nil, http.StatusNoContent)
	recs, _, err := events.ReadAfter(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		sources[fmt.Sprintf("the event %d %s", rec.Seq, rec.Type)], _ = json.Marshal(rec)
	}
	dataDir, _ := h.Listening["data_dir"].(string)
	h.Stop()
	if code := h.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0", code)
	}
	sources["the log of the second run"] = []byte(h.Log.String())
	maps.Copy(sources, dataFiles(t, dataDir))
	for where, content := range sources {
		for _, s := range sentinels {
			if bytes.Contains(content, []byte(s)) {
				t.Errorf("%s holds the value %s", where, s)
			}
		}
	}
}

func TestOwnerDelete_RemovesTheOwnersSecretsInItsTransaction(t *testing.T) {
	w := newWorld(t)
	// owner becomes the hub's only active admin, whom no delete removes.
	if a := w.do(t, w.Token, http.MethodPost, "/users/"+w.ownerID+"/promote", nil); a.status != http.StatusOK {
		t.Fatalf("promote owner = %d %s, want 200", a.status, a.body)
	}
	w.seed(t, secret.ScopeUser, w.outsiderID, "GIT_PAT", "NPM_TOKEN")
	w.seed(t, secret.ScopeOrg, w.orgID, "GIT_PAT", "NPM_TOKEN")
	w.seed(t, secret.ScopeUser, w.ownerID, "GIT_PAT", "NPM_TOKEN")
	scopes := []place{{secret.ScopeUser, w.outsiderID, ""}, {secret.ScopeOrg, w.orgID, ""}, {secret.ScopeUser, w.ownerID, ""}}
	// Each step is a delete, the status it answers and what the three
	// scopes hold afterwards.
	steps := []struct {
		path string
		want int
		left []int
	}{
		{"/users/" + w.outsiderID, http.StatusNoContent, []int{0, 2, 2}},
		{"/orgs/acme", http.StatusNoContent, []int{0, 0, 2}},
		{"/users/" + w.ownerID, http.StatusConflict, []int{0, 0, 2}},
	}
	for _, s := range steps {
		if a := w.do(t, w.Token, http.MethodDelete, s.path, nil); a.status != s.want {
			t.Fatalf("DELETE %s = %d %s, want %d", s.path, a.status, a.body, s.want)
		}
		for i, p := range scopes {
			if n := w.count(t, p.scope, p.id); n != s.left[i] {
				t.Errorf("after DELETE %s the %s scope %s holds %d secrets, want %d", s.path, p.scope, p.id, n, s.left[i])
			}
		}
	}
}
