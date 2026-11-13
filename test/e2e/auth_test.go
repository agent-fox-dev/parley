package e2e

import (
	"bytes"
	"context"
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
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// getAuthorized sends GET url with authorization as the Authorization
// header ("" sends none) and returns the status and the body as a JSON
// object.
func getAuthorized(t *testing.T, url, authorization string) (int, map[string]any) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		t.Fatal(err)
	}
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer res.Body.Close()
	var body map[string]any
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatalf("GET %s: the body is not a JSON object: %v", url, err)
	}
	return res.StatusCode, body
}

// addWhoami adds the non-public route GET /api/v1/test/whoami, which
// answers the caller's principal.
func addWhoami(c *hub.Composition) {
	c.AddRoute(server.Route{Method: http.MethodGet, Pattern: "/test/whoami", Handler: func(w http.ResponseWriter, r *http.Request) error {
		p, ok := identity.PrincipalOf(r.Context())
		if !ok {
			return server.Internal("no principal")
		}
		server.WriteJSON(w, http.StatusOK, p)
		return nil
	}})
}

func TestAuth_TheOperatorCallsTheHubWithTheAdminToken(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "[server]\nbind = \"127.0.0.1\"\nport = 0\n")
	env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir})

	var out, errb hubtest.LogBuffer
	if code := hub.Run(context.Background(), hub.Options{
		Args: []string{"--admin-email", "boss@x.io"}, Env: env, WorkDir: dir, Stdout: &out, Stderr: &errb,
	}); code != 0 {
		t.Fatalf("parleyd --admin-email boss@x.io returned %d, want 0; stderr = %q", code, errb.String())
	}
	tokenFile := filepath.Join(dir, "admin_token")
	token := checkTokenFile(t, tokenFile, dir)
	if err := os.Remove(tokenFile); err != nil {
		t.Fatal(err)
	}

	s := hubtest.Serve(t, hub.Options{Env: env, WorkDir: dir, Compose: addWhoami})
	whoami := s.URL("/api/v1/test/whoami")

	code, body := getAuthorized(t, whoami, "Bearer "+token)
	if code != http.StatusOK || body["type"] != "admin" || body["credential_id"] != "admin" || body["user"] != nil {
		t.Errorf("the admin token: %d %v, want 200 with type admin, credential_id admin and no user", code, body)
	}
	last := token[len(token)-1:]
	other := "0"
	if last == "0" {
		other = "1"
	}
	for _, c := range []struct{ name, authorization, message string }{
		{"a changed token", "Bearer " + token[:len(token)-1] + other, "invalid credentials"},
		{"no header", "", "authentication required"},
	} {
		code, body := getAuthorized(t, whoami, c.authorization)
		e, _ := body["error"].(map[string]any)
		if code != http.StatusUnauthorized || e["message"] != c.message {
			t.Errorf("%s: %d %v, want 401 %q", c.name, code, body, c.message)
		}
	}

	s.Stop()
	if code := s.ExitCode(t, 5*time.Second); code != 0 {
		t.Errorf("the hub returned %d after a stop, want 0", code)
	}
	hexPart := []byte(strings.TrimPrefix(token, "parley_admin_"))
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if bytes.Contains(data, hexPart) {
			t.Errorf("%s holds the admin token", path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
