package secret_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
			maps.Copy(env, tc.env)
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
			errs := secret.LogLines(t, log, "cannot start")
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
			if len(secret.LogLines(t, log, "listening")) != 0 {
				t.Errorf("the hub listened: %s", log)
			}
		})
	}
}

func TestStart_ReportsSecretEncryptionAndStartsWithoutAKey(t *testing.T) {
	_, current := newKey(t)
	// A previous key is reported by the starts of the key rotation test.
	cases := []struct {
		name       string
		cfg        hubtest.Config
		configured bool
	}{
		{"no key", hubtest.Config{}, false},
		{"current key from a key file relative to the config dir",
			hubtest.Config{TOML: "[secrets]\nkey_file = \"keys/current\"\n", Files: map[string]string{"keys/current": current + "\n"}}, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := hubtest.StartWith(t, tc.cfg)
			log := h.Log.String()
			reports := secret.LogLines(t, log, "secret encryption")
			if len(reports) != 1 {
				t.Fatalf("%d secret encryption lines, want 1; log = %s", len(reports), log)
			}
			want := map[string]any{"level": "INFO", "key_configured": tc.configured, "previous_key_present": false, "undecryptable_secrets": float64(0)}
			for k, v := range want {
				if reports[0][k] != v {
					t.Errorf("%s = %v, want %v", k, reports[0][k], v)
				}
			}
			warnings := secret.LogLines(t, log, noKeyWarning)
			if want := map[bool]int{false: 1}[tc.configured]; len(warnings) != want || want == 1 && warnings[0]["level"] != "WARN" {
				t.Errorf("no-key warnings %v, want %d at WARN; log = %s", warnings, want, log)
			}
			if status, body := h.Call(t, http.MethodGet, "/readyz", "", nil); status != http.StatusOK || !strings.Contains(string(body), `"ready"`) {
				t.Errorf("GET /readyz = %d %s, want 200 ready", status, body)
			}
		})
	}
}

func TestSecretValuesAndKeys_NeverLeaveTheHub(t *testing.T) {
	sentinels := []string{"S3ntinel-7f3a9c41-value", "S3ntinel-0b6e2d58-second"}
	currentRaw, current := newKey(t)
	previousRaw, previous := newKey(t)
	var secrets *secret.Component
	var events *event.Component
	ended := make(chan struct{}, 1)
	h := hubtest.StartWith(t, hubtest.Config{
		TOML: "[logging]\nlevel = \"trace\"\n",
		Env:  map[string]string{secret.EnvCurrentKey: current, secret.EnvPreviousKey: previous},
		Compose: func(c *hub.Composition) {
			secrets, events = c.Secrets(), c.Events()
			c.AddStartHook("test-seed-secret", func(ctx context.Context, rt *hub.Runtime) error {
				return rt.Store.Tx(ctx, func(tx *sql.Tx) error {
					return c.Secrets().Put(ctx, tx, secret.ScopeUser, "u1", "GIT_PAT", "ghp_value")
				})
			})
			passEnd(c, ended)
		},
	})
	// The first maintenance pass, which reads the stored secret under both
	// keys, ends before anything else.
	waitPass(t, ended)
	userID := h.CreateUser(t, "owner")
	cred := h.IssueKey(t, userID)
	ctx := context.Background()
	// sources are every text the hub gave out or kept, by where it was.
	sources := map[string][]byte{}
	keep := func(what string, status int, answer []byte, want int) {
		t.Helper()
		if status != want {
			t.Errorf("%s = %d %s, want %d", what, status, answer, want)
		}
		sources[fmt.Sprintf("the answer %d to %s", len(sources), what)] = answer
	}
	call := func(method, path string, body any, want int) {
		t.Helper()
		status, answer := h.Call(t, method, "/api/v1"+path, cred, body)
		keep(method+" "+path, status, answer, want)
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
	raw, err := json.Marshal(create)
	if err != nil {
		t.Fatal(err)
	}
	// The create with an Idempotency-Key, then its replay.
	for _, want := range []int{http.StatusCreated, http.StatusOK} {
		req, err := http.NewRequest(http.MethodPost, h.URL("/api/v1/user/secrets"), bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer "+cred)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "create-1")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		answer, err := io.ReadAll(res.Body)
		if cerr := res.Body.Close(); err != nil || cerr != nil {
			t.Fatal(err, cerr)
		}
		keep("POST /user/secrets with an Idempotency-Key", res.StatusCode, answer, want)
	}
	call(http.MethodGet, "/user/secrets", nil, http.StatusOK)
	call(http.MethodPost, "/user/secrets", create, http.StatusConflict)
	get(sentinels[0])
	call(http.MethodPatch, "/user/secrets/GIT_PAT", map[string]any{"value": sentinels[1]}, http.StatusOK)
	get(sentinels[1])
	sources["the log of the first run"] = []byte(h.Log.String())
	_, other := newKey(t)
	h.RestartEnv(t, map[string]string{secret.EnvCurrentKey: other})
	get("unreadable")
	call(http.MethodDelete, "/user/secrets/GIT_PAT", nil, http.StatusNoContent)
	recs, _, err := events.ReadAfter(ctx, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		if sources[fmt.Sprintf("the event %d %s", rec.Seq, rec.Type)], err = json.Marshal(rec); err != nil {
			t.Fatal(err)
		}
	}
	maps.Copy(sources, stopAndRead(t, h))
	noneHolds(t, sources, map[string][]byte{
		"the first value": []byte(sentinels[0]), "the second value": []byte(sentinels[1]),
		"the current key in base64": []byte(current), "the current key's bytes": currentRaw,
		"the previous key in base64": []byte(previous), "the previous key's bytes": previousRaw,
	})
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
		return pageOf(t, answer{http.StatusOK, []byte(parley("", "secrets", "list", "--workspace", "acme"))}).Items
	}
	_, seq := w.eventsAfter(t, 0)

	var created struct {
		Results []struct {
			Scope string
			Items []struct{ Key string }
		}
	}
	if err := json.Unmarshal([]byte(parley("", "secrets", "create", "GIT_PAT="+values[0], "--workspace", "acme")), &created); err != nil {
		t.Fatal(err)
	}
	if got := fmt.Sprintf("%+v", created); got != "{Results:[{Scope:workspace Items:[{Key:GIT_PAT}]}]}" {
		t.Fatalf("create printed %s, want scope workspace with the item GIT_PAT", got)
	}
	read(values[0], true)
	items := listed()
	if len(items) != 1 {
		t.Fatalf("list printed %v, want one item", items)
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

	sources := stopAndRead(t, w.Hub)
	for i, out := range outputs {
		sources[fmt.Sprintf("command output %d", i)] = []byte(out)
	}
	noneHolds(t, sources, map[string][]byte{"the created value": []byte(values[0]), "the piped value": []byte(values[1])})
}

func TestKeyRotation_ReencryptsUnderTheNewKeyAndKeepsEverySecret(t *testing.T) {
	_, keyA := newKey(t)
	_, keyB := newKey(t)
	var secrets *secret.Component
	ended := make(chan struct{}, 1)
	h := hubtest.StartWith(t, hubtest.Config{
		Env:     map[string]string{secret.EnvCurrentKey: keyA},
		Compose: func(c *hub.Composition) { secrets = c.Secrets(); passEnd(c, ended) },
	})
	// The first pass of each run is waited for before the next restart, so
	// that the mark a pass leaves is the one of the run that is waited for.
	waitPass(t, ended)
	ctx := context.Background()
	places := []struct {
		scope     secret.Scope
		id, value string
	}{
		{secret.ScopeUser, "u1", "user-value-1d"}, {secret.ScopeOrg, "o1", "org-value-2e"},
		{secret.ScopeWorkspace, "acme", "workspace-value-3f"},
	}
	listed := func() (all []secret.Item) {
		t.Helper()
		for _, p := range places {
			items, err := secrets.ListPage(ctx, p.scope, p.id, "", 10)
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, items...)
		}
		return all
	}
	// restart starts the hub again with the keys given, and checks its start
	// report.
	restart := func(vars map[string]string, undecryptable float64) {
		t.Helper()
		h.RestartEnv(t, vars)
		lines := secret.LogLines(t, h.Log.String(), "secret encryption")
		if len(lines) != 1 || lines[0]["key_configured"] != true || lines[0]["previous_key_present"] != (vars[secret.EnvPreviousKey] != "") ||
			lines[0]["undecryptable_secrets"] != undecryptable {
			t.Fatalf("secret encryption lines = %v, want one with a key, the previous key %v, %v undecryptable", lines, vars, undecryptable)
		}
	}
	err := h.Store.Tx(ctx, func(tx *sql.Tx) error {
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
	before := listed()

	restart(map[string]string{secret.EnvCurrentKey: keyB, secret.EnvPreviousKey: keyA}, 0)
	waitPass(t, ended)
	if lines := secret.LogLines(t, h.Log.String(), "secret re-encryption"); len(lines) != 1 ||
		lines[0]["converted"] != float64(3) || lines[0]["remaining"] != float64(0) {
		t.Fatalf("secret re-encryption lines = %v, want one with converted 3 and remaining 0", lines)
	}

	restart(map[string]string{secret.EnvCurrentKey: keyB}, 0)
	for _, p := range places {
		if v, found, err := secrets.Get(ctx, p.scope, p.id, "TOKEN"); err != nil || !found || v != p.value {
			t.Errorf("Get(%s %s) under the new key alone = %q, %v, %v; want %q", p.scope, p.id, v, found, err, p.value)
		}
	}
	if after := listed(); fmt.Sprint(after) != fmt.Sprint(before) {
		t.Errorf("the entries after the rotation = %v, want them unchanged: %v", after, before)
	}

	restart(map[string]string{secret.EnvCurrentKey: keyA}, 3)
	if n := len(listed()); n != 3 {
		t.Errorf("under the old key alone %d entries are listed, want 3", n)
	}
}

func TestOrgSecrets_AreSharedWhileAWorkspacesSecretsStayWithItsOwner(t *testing.T) {
	w := newWorld(t)
	_, seq := w.eventsAfter(t, 0)
	// A step answered 200 lists keys; one answered 4xx has errorType.
	steps := []struct {
		cred, method, path string
		body               any
		want               int
		errorType, keys    string
	}{
		{w.memberKey, http.MethodPost, "/orgs/acme/secrets", entries(map[string]any{"key": "NPM_TOKEN", "value": "npm-7a1c"}), http.StatusCreated, "", ""},
		{w.member2Key, http.MethodGet, "/orgs/acme/secrets", nil, http.StatusOK, "", "[NPM_TOKEN]"},
		{w.member2Key, http.MethodGet, "/workspaces/acme/secrets", nil, http.StatusForbidden, "forbidden", ""},
		{w.ownerKey, http.MethodGet, "/workspaces/acme/secrets", nil, http.StatusOK, "", "[]"},
		{w.outsiderKey, http.MethodGet, "/orgs/acme/secrets", nil, http.StatusNotFound, "not_found", ""},
		{w.outsiderKey, http.MethodGet, "/workspaces/acme/secrets", nil, http.StatusNotFound, "not_found", ""},
	}
	for _, s := range steps {
		a := w.do(t, s.cred, s.method, s.path, s.body)
		if a.status != s.want {
			t.Fatalf("%s %s = %d %s, want %d", s.method, s.path, a.status, a.body, s.want)
		}
		switch {
		case s.want >= 400:
			if et, _ := errorOf(t, a); et != s.errorType {
				t.Errorf("%s %s error_type = %s, want %s", s.method, s.path, et, s.errorType)
			}
		case s.want == http.StatusOK:
			if keys := fmt.Sprint(pageOf(t, a).keys(t)); keys != s.keys {
				t.Errorf("%s %s lists %s, want %s", s.method, s.path, keys, s.keys)
			}
		}
	}
	recs, _ := w.eventsAfter(t, seq)
	var scopes []string
	for _, rec := range recs {
		if strings.HasPrefix(rec.Type, "parley.secret.") {
			scopes = append(scopes, metadataOf(t, rec)["scope"])
		}
	}
	if fmt.Sprint(scopes) != "[org]" {
		t.Errorf("the scopes of the secret events = %v, want exactly one, org", scopes)
	}
}
