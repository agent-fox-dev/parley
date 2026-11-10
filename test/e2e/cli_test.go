package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/identity"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// recordedHub is the hub's real handler (the conventions of package 02 and
// the authenticator of package 04 over a store) behind a handler that
// records each request's path and Authorization header.
type recordedHub struct {
	*httptest.Server
	mu   sync.Mutex
	seen []struct{ path, auth string }
}

func newRecordedHub(t *testing.T) *recordedHub {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(context.Background(), store.Options{DatabasePath: filepath.Join(dir, "parley.db"), WorkspaceRoot: filepath.Join(dir, "ws")},
		append([]store.Step{identity.AdminSettingsStep}, identity.Steps...))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	opts := server.DefaultOptions()
	opts.Logger = slog.New(slog.NewJSONHandler(io.Discard, nil))
	srv := server.New(opts)
	srv.SetAuthenticator(identity.NewAuthenticator(st, nil))
	h := &recordedHub{}
	inner := srv.Handler()
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.seen = append(h.seen, struct{ path, auth string }{r.URL.Path, r.Header.Get("Authorization")})
		h.mu.Unlock()
		inner.ServeHTTP(w, r)
	}))
	t.Cleanup(h.Close)
	return h
}

func TestCLI_VersionFindsTheHubThroughTheConfigurationFile(t *testing.T) {
	hub := newRecordedHub(t)
	dir := t.TempDir()
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	t.Setenv("PARLEY_ENDPOINT_URL", "")
	t.Setenv("PARLEY_API_KEY", "")
	key := "parley_ABCDabcd_" + strings.Repeat("Q", 32)
	if err := cli.SaveSettings(cli.Settings{EndpointURL: hub.URL + "/", APIKey: key, UserID: "u1"}); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "config.toml")
	before, _ := os.ReadFile(file)
	status, want := getJSON(t, hub.URL+"/version", "")
	if status != 200 {
		t.Fatalf("/version = %d", status)
	}
	var stdout, stderr bytes.Buffer
	if code := cli.Run(context.Background(), []string{"version"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var doc struct {
		Server map[string]any `json:"server"`
	}
	json.Unmarshal(stdout.Bytes(), &doc)
	if len(doc.Server) == 0 || doc.Server["version"] != want["version"] || doc.Server["go_version"] != want["go_version"] || stderr.Len() != 0 {
		t.Errorf("server %v, stderr %q", doc.Server, stderr.String())
	}
	after, _ := os.ReadFile(file)
	fi, _ := os.Stat(file)
	if !bytes.Equal(before, after) || fi.Mode().Perm() != 0o600 {
		t.Error("the configuration file changed")
	}
}

func TestCLI_TheHubsRefusalReachesStderr(t *testing.T) {
	hub := newRecordedHub(t)
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	t.Setenv("PARLEY_ENDPOINT_URL", "")
	t.Setenv("PARLEY_API_KEY", "")
	key := "parley_ABCDabcd_" + strings.Repeat("Q", 32)
	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"login", "--api-key", key, "--endpoint-url", hub.URL}, &stdout, &stderr)
	if code != 1 || stdout.Len() != 0 {
		t.Fatalf("exit %d stdout %q", code, stdout.String())
	}
	var doc struct {
		Error struct {
			Code      int    `json:"code"`
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &doc); err != nil {
		t.Fatalf("stderr %q", stderr.String())
	}
	hub.mu.Lock()
	seen := append([]struct{ path, auth string }(nil), hub.seen...)
	hub.mu.Unlock()
	if len(seen) != 1 || !strings.HasPrefix(seen[0].path, "/api/v1/") || seen[0].auth != "Bearer "+key {
		t.Errorf("hub saw %+v", seen)
	}
	if doc.Error.Code < 400 || (doc.Error.ErrorType != "unauthenticated" && doc.Error.ErrorType != "not_found") {
		t.Errorf("envelope %+v", doc.Error)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the configuration directory was created")
	}
}
