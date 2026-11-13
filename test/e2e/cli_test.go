package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
)

// The tests of this file set environment variables, so they must not run in
// parallel.

// serveHub serves a hub with no identity provider on a free loopback port
// and returns its URL.
func serveHub(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	writeConfig(t, dir, "[server]\nbind = \"127.0.0.1\"\nport = 0\n")
	env := config.MapEnv(map[string]string{"XDG_CONFIG_HOME": dir, "XDG_DATA_HOME": dir})
	return serve(t, hub.Options{Env: env, WorkDir: dir}).url("")
}

// clientEnv points PARLEY_CONFIG_DIR at configDir and unsets
// PARLEY_ENDPOINT_URL and PARLEY_API_KEY.
func clientEnv(t *testing.T, configDir string) {
	t.Helper()
	t.Setenv("PARLEY_CONFIG_DIR", configDir)
	for _, key := range []string{"PARLEY_ENDPOINT_URL", "PARLEY_API_KEY"} {
		t.Setenv(key, "")
		os.Unsetenv(key)
	}
}

// unknownAPIKey is a well-formed API key no hub issued.
func unknownAPIKey() string {
	const alphanumerics = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	draw := func(n int) string {
		b := make([]byte, n)
		for i := range b {
			b[i] = alphanumerics[rand.IntN(len(alphanumerics))]
		}
		return string(b)
	}
	return "parley_" + draw(8) + "_" + draw(32)
}

func TestCLI_VersionFindsTheHubThroughTheConfigurationFile(t *testing.T) {
	hubURL := serveHub(t)
	configDir := t.TempDir()
	clientEnv(t, configDir)
	// The file as parley login's save writes it: the three keys encoded by
	// the same TOML encoder, mode 0600. The e2e package cannot call the
	// unexported save, and a login saves neither a trailing slash nor a key
	// the hub does not know (docs/errata/09_cli_foundation.md).
	path := filepath.Join(configDir, "config.toml")
	saved, err := toml.Marshal(map[string]any{"endpoint_url": hubURL + "/", "api_key": unknownAPIKey(), "user_id": "u-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, saved, 0o600); err != nil {
		t.Fatal(err)
	}
	res, err := http.Get(hubURL + "/version")
	if err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	err = json.NewDecoder(res.Body).Decode(&want)
	res.Body.Close()
	if err != nil {
		t.Fatalf("GET /version: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"version"}, &stdout, &stderr)

	if code != 0 {
		t.Errorf("parley version returned %d, want 0", code)
	}
	var doc struct {
		Server map[string]any `json:"server"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil || !reflect.DeepEqual(doc.Server, want) {
		t.Errorf("stdout = %q (%v), want a server member equal to %v", stdout.String(), err, want)
	}
	if stderr.Len() != 0 {
		t.Errorf("stderr = %q, want it empty", stderr.String())
	}
	if got, err := os.ReadFile(path); err != nil || !bytes.Equal(got, saved) {
		t.Errorf("config file = %q (%v), want its bytes unchanged, %q", got, err, saved)
	}
	if fi, err := os.Stat(path); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("config file mode: %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

// proxied is a request the recording proxy passed to the hub, with the
// status the hub answered.
type proxied struct {
	path, authorization string
	status              int
}

// recordingProxy serves a reverse proxy to target on a free loopback port and
// records every request it passes on.
type recordingProxy struct {
	*httptest.Server

	mu       sync.Mutex
	requests []proxied
}

func newRecordingProxy(t *testing.T, target string) *recordingProxy {
	t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		t.Fatal(err)
	}
	p := &recordingProxy{}
	rp := httputil.NewSingleHostReverseProxy(u)
	p.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w}
		rp.ServeHTTP(rec, r)
		p.mu.Lock()
		p.requests = append(p.requests, proxied{path: r.URL.Path, authorization: r.Header.Get("Authorization"), status: rec.status})
		p.mu.Unlock()
	}))
	t.Cleanup(p.Close)
	return p
}

func (p *recordingProxy) recorded() []proxied {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]proxied(nil), p.requests...)
}

// statusRecorder is a ResponseWriter that keeps the status written to it.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(status int) {
	r.status = status
	r.ResponseWriter.WriteHeader(status)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func TestCLI_LoginPrintsTheHubsRefusalOfAnUnknownKeyAsReceived(t *testing.T) {
	proxy := newRecordingProxy(t, serveHub(t))
	configDir := filepath.Join(t.TempDir(), "parley")
	clientEnv(t, configDir)
	key := unknownAPIKey()

	var stdout, stderr bytes.Buffer
	code := cli.Run(context.Background(), []string{"login", "--api-key", key, "--endpoint-url", proxy.URL}, &stdout, &stderr)

	if code != 1 {
		t.Errorf("parley login returned %d, want 1", code)
	}
	if stdout.Len() != 0 {
		t.Errorf("stdout = %q, want it empty", stdout.String())
	}
	var api []proxied
	for _, r := range proxy.recorded() {
		if strings.HasPrefix(r.path, "/api/v1/") {
			api = append(api, r)
		}
	}
	if len(api) != 1 || api[0].authorization != "Bearer "+key {
		t.Fatalf("the hub got %+v under /api/v1, want one request with Authorization Bearer <the key>", api)
	}
	var envelope struct {
		Error struct {
			Code      int    `json:"code"`
			ErrorType string `json:"error_type"`
		} `json:"error"`
	}
	dec := json.NewDecoder(&stderr)
	if err := dec.Decode(&envelope); err != nil {
		t.Fatalf("stderr is not an error envelope: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		t.Errorf("stderr holds more than one JSON document")
	}
	if e := envelope.Error; e.Code != api[0].status || e.Code < 400 || (e.ErrorType != "unauthenticated" && e.ErrorType != "not_found") {
		t.Errorf("envelope code %d, error_type %q, want the hub's status %d (400 or above) and unauthenticated or not_found", e.Code, e.ErrorType, api[0].status)
	}
	if _, err := os.Lstat(configDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("%s exists (or cannot be checked: %v), want it absent", configDir, err)
	}
}
