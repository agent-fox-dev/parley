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

	"github.com/agent-fox-dev/parley/internal/cli"
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

func TestSecretsFromTheShell_StoreUpdateAndRemoveAWorkspaceSecret(t *testing.T) {
	// The CLI reads its config file only for what the flags do not give.
	t.Setenv("PARLEY_CONFIG_DIR", t.TempDir())
	w := newWorld(t)
	values := []string{"ghp-first-5c2e91a7", "ghp-piped-8d40b3f6"}
	ctx := context.Background()
	var outputs []string
	parley := func(stdin string, args ...string) string {
		t.Helper()
		var stdout, stderr bytes.Buffer
		args = append(args, "--endpoint-url", w.URL(""), "--api-key", w.ownerKey)
		if code := cli.RunWith(ctx, args, &stdout, &stderr, cli.Hooks{Stdin: strings.NewReader(stdin)}); code != 0 {
			t.Fatalf("parley %v = %d, stderr %s", args, code, stderr.String())
		}
		outputs = append(outputs, stdout.String(), stderr.String())
		return stdout.String()
	}
	read := func(want string, wantFound bool) {
		t.Helper()
		v, found, err := w.secrets.Get(ctx, secret.ScopeWorkspace, "acme", "GIT_PAT")
		if err != nil || found != wantFound || v != want {
			t.Fatalf("Get = %q, %v, %v; want %q, %v", v, found, err, want, wantFound)
		}
	}
	listed := func() []map[string]json.RawMessage {
		t.Helper()
		var doc struct {
			Items []map[string]json.RawMessage `json:"items"`
		}
		if err := json.Unmarshal([]byte(parley("", "secrets", "list", "--workspace", "acme")), &doc); err != nil {
			t.Fatal(err)
		}
		return doc.Items
	}
	_, seq := w.eventsAfter(t, 0)

	var created struct {
		Results []struct {
			Scope string `json:"scope"`
			Items []struct {
				Key string `json:"key"`
			} `json:"items"`
		} `json:"results"`
	}
	if err := json.Unmarshal([]byte(parley("", "secrets", "create", "GIT_PAT="+values[0], "--workspace", "acme")), &created); err != nil {
		t.Fatal(err)
	}
	if len(created.Results) != 1 || created.Results[0].Scope != "workspace" ||
		len(created.Results[0].Items) != 1 || created.Results[0].Items[0].Key != "GIT_PAT" {
		t.Fatalf("create printed %+v, want scope workspace with the item GIT_PAT", created)
	}
	read(values[0], true)
	items := listed()
	if len(items) != 1 || len(items[0]) != 3 || items[0]["key"] == nil || items[0]["created_at"] == nil || items[0]["updated_at"] == nil {
		t.Fatalf("list printed %v, want one item of exactly key, created_at and updated_at", items)
	}
	parley(values[1]+"\n", "secrets", "update", "GIT_PAT", "--from-stdin", "--workspace", "acme")
	read(values[1], true)
	parley("", "secrets", "delete", "GIT_PAT", "--workspace", "acme")
	if items := listed(); len(items) != 0 {
		t.Fatalf("list after delete printed %v, want none", items)
	}
	read("", false)

	recs, _ := w.eventsAfter(t, seq)
	var types []string
	for _, rec := range recs {
		if strings.HasPrefix(rec.Type, "parley.secret.") {
			if rec.ResourceID != "GIT_PAT" || !equalPtr(rec.Workspace, ptr("acme")) {
				t.Errorf("event %s is about %s in %v, want GIT_PAT in acme", rec.Type, rec.ResourceID, rec.Workspace)
			}
			types = append(types, rec.Type)
		}
	}
	if want := []string{"parley.secret.create", "parley.secret.update", "parley.secret.delete"}; fmt.Sprint(types) != fmt.Sprint(want) {
		t.Errorf("secret events = %v, want %v", types, want)
	}

	dataDir, _ := w.Listening["data_dir"].(string)
	w.Stop()
	if code := w.ExitCode(t, 10*time.Second); code != 0 {
		t.Fatalf("the hub stopped with %d, want 0", code)
	}
	sources := dataFiles(t, dataDir)
	sources["the log"] = []byte(w.Log.String())
	for i, out := range outputs {
		sources[fmt.Sprintf("command output %d", i)] = []byte(out)
	}
	for where, content := range sources {
		for _, v := range values {
			if bytes.Contains(content, []byte(v)) {
				t.Errorf("%s holds the value %s", where, v)
			}
		}
	}
}

func TestKeyRotation_ReencryptsUnderTheNewKeyAndKeepsEverySecret(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[server]\nbind = \"127.0.0.1\"\nport = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, keyA := newKey(t)
	_, keyB := newKey(t)
	envOf := func(vars map[string]string) config.Env {
		m := map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir}
		maps.Copy(m, vars)
		return config.MapEnv(m)
	}
	ctx := context.Background()
	if code := hub.Run(ctx, hub.Options{Args: []string{"--admin-email", "boss@example.com"},
		Env: envOf(map[string]string{secret.EnvCurrentKey: keyA}), WorkDir: dir}); code != 0 {
		t.Fatalf("bootstrap returned %d, want 0", code)
	}
	// The hub does not start while the token file of the bootstrap is there.
	if err := os.Remove(filepath.Join(dir, "admin_token")); err != nil {
		t.Fatal(err)
	}
	// run starts the hub with the variables vars until its first
	// maintenance pass has ended, and returns it with its secret component
	// and its store.
	run := func(vars map[string]string) (*hubtest.Served, *secret.Component, *hub.Runtime) {
		t.Helper()
		var secrets *secret.Component
		var rt *hub.Runtime
		passEnded := make(chan struct{})
		s := hubtest.Serve(t, hub.Options{Env: envOf(vars), WorkDir: dir, Compose: func(c *hub.Composition) {
			secrets, rt = c.Secrets(), c.Runtime()
			// Tasks registered after the hub's own run last in a pass.
			c.AddMaintenanceTask("test-pass-end", func(context.Context, *hub.Runtime) error {
				select {
				case <-passEnded:
				default:
					close(passEnded)
				}
				return nil
			})
		}})
		select {
		case <-passEnded:
		case <-time.After(5 * time.Second):
			t.Fatal("the first maintenance pass did not end within 5 s")
		}
		return s, secrets, rt
	}
	stop := func(s *hubtest.Served) {
		t.Helper()
		s.Stop()
		if code := s.ExitCode(t, 10*time.Second); code != 0 {
			t.Fatalf("the hub stopped with %d, want 0; log = %s", code, s.Log.String())
		}
	}
	report := func(s *hubtest.Served, previous bool, undecryptable float64) {
		t.Helper()
		lines := logLines(t, s.Log.String(), "secret encryption")
		if len(lines) != 1 || lines[0]["key_configured"] != true || lines[0]["previous_key_present"] != previous ||
			lines[0]["undecryptable_secrets"] != undecryptable {
			t.Fatalf("secret encryption lines = %v, want one with a key, previous %v, %v undecryptable", lines, previous, undecryptable)
		}
	}
	places := []struct {
		scope     secret.Scope
		id, value string
	}{
		{secret.ScopeUser, "u1", "user-value-1d"}, {secret.ScopeOrg, "o1", "org-value-2e"},
		{secret.ScopeWorkspace, "acme", "workspace-value-3f"},
	}
	listed := func(secrets *secret.Component) []secret.Item {
		t.Helper()
		var all []secret.Item
		for _, p := range places {
			items, err := secrets.ListPage(ctx, p.scope, p.id, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, items...)
		}
		return all
	}

	s, secrets, rt := run(map[string]string{secret.EnvCurrentKey: keyA})
	err := rt.Store.Tx(ctx, func(tx *sql.Tx) error {
		for _, p := range places {
			if err := secrets.Put(ctx, tx, p.scope, p.id, "TOKEN", p.value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	before := listed(secrets)
	stop(s)

	s, _, _ = run(map[string]string{secret.EnvCurrentKey: keyB, secret.EnvPreviousKey: keyA})
	report(s, true, 0)
	if lines := logLines(t, s.Log.String(), "secret re-encryption"); len(lines) != 1 ||
		lines[0]["converted"] != float64(3) || lines[0]["remaining"] != float64(0) {
		t.Fatalf("secret re-encryption lines = %v, want one with converted 3 and remaining 0", lines)
	}
	stop(s)

	s, secrets, _ = run(map[string]string{secret.EnvCurrentKey: keyB})
	report(s, false, 0)
	for _, p := range places {
		if v, found, err := secrets.Get(ctx, p.scope, p.id, "TOKEN"); err != nil || !found || v != p.value {
			t.Errorf("Get(%s %s) under the new key alone = %q, %v, %v; want %q", p.scope, p.id, v, found, err, p.value)
		}
	}
	if after := listed(secrets); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the entries after the rotation = %v, want them unchanged: %v", after, before)
	}
	stop(s)

	s, secrets, _ = run(map[string]string{secret.EnvCurrentKey: keyA})
	report(s, false, 3)
	if n := len(listed(secrets)); n != 3 {
		t.Errorf("under the old key alone %d entries are listed, want 3", n)
	}
	stop(s)
}

func TestOrgSecrets_AreSharedWhileAWorkspacesSecretsStayWithItsOwner(t *testing.T) {
	w := newWorld(t)
	_, seq := w.eventsAfter(t, 0)
	steps := []struct {
		cred, method, path string
		body               any
		want               int
		errorType          string
	}{
		{w.memberKey, http.MethodPost, "/orgs/acme/secrets", entries(map[string]any{"key": "NPM_TOKEN", "value": "npm-7a1c"}), http.StatusCreated, ""},
		{w.member2Key, http.MethodGet, "/orgs/acme/secrets", nil, http.StatusOK, ""},
		{w.member2Key, http.MethodGet, "/workspaces/acme/secrets", nil, http.StatusForbidden, "forbidden"},
		{w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil, http.StatusOK, ""},
		{w.outsiderKey, http.MethodGet, "/orgs/acme/secrets", nil, http.StatusNotFound, "not_found"},
		{w.outsiderKey, http.MethodGet, "/workspaces/acme/secrets", nil, http.StatusNotFound, "not_found"},
	}
	var answers []answer
	for _, s := range steps {
		a := w.do(t, s.cred, s.method, s.path, s.body)
		if a.status != s.want {
			t.Fatalf("%s %s = %d %s, want %d", s.method, s.path, a.status, a.body, s.want)
		}
		if s.errorType != "" {
			if et, _ := errorOf(t, a); et != s.errorType {
				t.Errorf("%s %s error_type = %s, want %s", s.method, s.path, et, s.errorType)
			}
		}
		answers = append(answers, a)
	}
	if keys := pageOf(t, answers[1]).keys(t); fmt.Sprint(keys) != "[NPM_TOKEN]" {
		t.Errorf("the second member lists %v in the org, want [NPM_TOKEN]", keys)
	}
	if keys := pageOf(t, answers[3]).keys(t); len(keys) != 0 {
		t.Errorf("the owner lists %v in the workspace, want none", keys)
	}
	recs, _ := w.eventsAfter(t, seq)
	var scopes []string
	for _, rec := range recs {
		if strings.HasPrefix(rec.Type, "parley.secret.") {
			var meta struct {
				Scope string `json:"scope"`
			}
			_ = json.Unmarshal(rec.Metadata, &meta)
			scopes = append(scopes, meta.Scope)
		}
	}
	if fmt.Sprint(scopes) != "[org]" {
		t.Errorf("the scopes of the secret events = %v, want exactly one, org", scopes)
	}
}
