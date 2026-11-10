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
// records each request's path, Authorization header and answered status.
type recordedHub struct {
	*httptest.Server
	mu   sync.Mutex
	seen []seenRequest
}

type seenRequest struct {
	path, auth string
	status     int
}

// statusRecorder keeps the status a handler answers with.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (h *recordedHub) requests() []seenRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]seenRequest(nil), h.seen...)
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
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		inner.ServeHTTP(rec, r)
		h.mu.Lock()
		h.seen = append(h.seen, seenRequest{r.URL.Path, r.Header.Get("Authorization"), rec.status})
		h.mu.Unlock()
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
	// The test's own GET /version and the CLI's: neither carries a credential.
	seen := hub.requests()
	if len(seen) != 2 {
		t.Errorf("hub saw %+v", seen)
	}
	for _, r := range seen {
		if r.path != "/version" || r.auth != "" {
			t.Errorf("request %+v, want GET /version without a credential", r)
		}
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
	seen := hub.requests()
	if len(seen) != 1 || !strings.HasPrefix(seen[0].path, "/api/v1/") || seen[0].auth != "Bearer "+key {
		t.Fatalf("hub saw %+v", seen)
	}
	if doc.Error.Code != seen[0].status || doc.Error.Code < 400 || (doc.Error.ErrorType != "unauthenticated" && doc.Error.ErrorType != "not_found") {
		t.Errorf("envelope %+v, hub answered %d", doc.Error, seen[0].status)
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("the configuration directory was created")
	}
}
