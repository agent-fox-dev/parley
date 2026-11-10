package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
)

// whoamiRoute adds the test route GET /api/v1/test/whoami, which answers
// the caller.
func whoamiRoute(c *hub.Composition) {
	c.OnServer(func(_ *hub.Runtime, srv *server.Server) error {
		srv.Register(server.Route{Method: "GET", Pattern: "/test/whoami", Handler: func(w http.ResponseWriter, r *http.Request) error {
			server.WriteJSON(w, 200, identity.PrincipalFrom(r.Context()))
			return nil
		}})
		return nil
	})
}

func getJSON(t *testing.T, url, auth string) (int, map[string]any) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if auth != "" {
		req.Header.Set("Authorization", auth)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	_ = json.Unmarshal(b, &doc)
	return resp.StatusCode, doc
}

func errorMessage(doc map[string]any) any {
	e, _ := doc["error"].(map[string]any)
	return e["message"]
}

func TestAuth_AnOperatorCallsTheHubWithTheAdminToken(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "config.toml"), []byte("[server]\nbind = \"127.0.0.1\"\nport = 0\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out, errb lockedBuffer
	code := hub.Run(context.Background(), hub.Options{Args: []string{"--admin-email", "boss@x.io"}, Env: config.MapEnv(nil), WorkDir: dir, Stdout: &out, Stderr: &errb})
	if code != 0 {
		t.Fatalf("bootstrap exit %d: %s", code, errb.String())
	}
	raw, err := os.ReadFile(filepath.Join(dir, "admin_token"))
	if err != nil {
		t.Fatal(err)
	}
	token := string(raw)
	os.Remove(filepath.Join(dir, "admin_token"))

	addr, stop := inProcessHub(t, dir, whoamiRoute, nil)
	url := "http://" + addr + "/api/v1/test/whoami"
	status, doc := getJSON(t, url, "Bearer "+token)
	if status != 200 || doc["type"] != "admin" || doc["credential_id"] != "admin" || doc["user"] != nil {
		t.Errorf("admin token: %d %v", status, doc)
	}
	last := token[len(token)-1]
	repl := byte('0')
	if last == '0' {
		repl = '1'
	}
	bad := token[:len(token)-1] + string(repl)
	if status, doc := getJSON(t, url, "Bearer "+bad); status != 401 || errorMessage(doc) != "invalid credentials" {
		t.Errorf("changed token: %d %v", status, doc)
	}
	if status, doc := getJSON(t, url, ""); status != 401 || errorMessage(doc) != "authentication required" {
		t.Errorf("no header: %d %v", status, doc)
	}
	stop()
	filepath.WalkDir(filepath.Join(dir, "data"), func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if b, _ := os.ReadFile(p); bytes.Contains(b, []byte(strings.TrimPrefix(token, "parley_admin_"))) {
				t.Errorf("%s holds the admin token", p)
			}
		}
		return nil
	})
}
