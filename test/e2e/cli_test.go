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
	"strings"
	"sync"
	"testing"

	"github.com/agent-fox-dev/parley/internal/cli"
	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/hub"
	"github.com/agent-fox-dev/parley/internal/hubtest"
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
	return hubtest.Serve(t, hub.Options{Env: env, WorkDir: dir}).URL("")
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
