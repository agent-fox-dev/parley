package e2e

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/BurntSushi/toml"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

func TestKeys_ADeveloperRefreshesTheKeyOnTheirMachine(t *testing.T) {
	h := startIdentityHub(t)
	h.createUser(t, "alice")
	old := h.issueKey(t, "alice")
	alice, err := identity.UserBySelector(context.Background(), h.store.Main(), "alice")
	if err != nil {
		t.Fatal(err)
	}
	// The developer's machine: a config file as parley login leaves it, and
	// no flag or variable in its way.
	dir := t.TempDir()
	clientEnv(t, dir)
	file := filepath.Join(dir, "config.toml")
	writeTOML(t, file, map[string]string{"endpoint_url": h.url, "api_key": old, "user_id": alice.ID})
	parley := func(args ...string) ran {
		var stdout, stderr bytes.Buffer
		code := cli.Run(context.Background(), args, &stdout, &stderr)
		return ran{code, stdout.String(), stderr.String()}
	}

	refreshed := parley("keys", "refresh")
	if refreshed.code != 0 {
		t.Fatalf("keys refresh exited %d; stderr = %q", refreshed.code, refreshed.stderr)
	}
	out := refreshed.json(t)
	keyID := old[len("parley_") : len("parley_")+8]
	if out["key"] == old || out["key_id"] != keyID {
		t.Errorf("keys refresh printed %v, want a new key with key_id %s", out, keyID)
	}
	var saved map[string]string
	if _, err := toml.DecodeFile(file, &saved); err != nil {
		t.Fatal(err)
	}
	if saved["api_key"] != out["key"] || saved["api_key"] == old {
		t.Errorf("the config file's api_key = %q, want the new key %v", saved["api_key"], out["key"])
	}

	if status, _ := getAuthorized(t, h.url+"/api/v1/user/keys", "Bearer "+old); status != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/user/keys with the old key = %d, want 401", status)
	}
	list := parley("keys", "list")
	if list.code != 0 {
		t.Fatalf("keys list with the saved key exited %d; stderr = %q", list.code, list.stderr)
	}
	ids, revoked := itemValues(t, list, "key_id"), itemValues(t, list, "revoked_at")
	if len(ids) != 1 || ids[0] != keyID || revoked[0] != nil {
		t.Errorf("keys list = key_ids %v, revoked_at %v; want the one key %s, not revoked", ids, revoked, keyID)
	}
}

// writeTOML writes values to path as a TOML file with mode 0600.
func writeTOML(t *testing.T, path string, values map[string]string) {
	t.Helper()
	var b bytes.Buffer
	if err := toml.NewEncoder(&b).Encode(values); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
}
