package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
)

func setBuild(t *testing.T, version, commit string) {
	t.Helper()
	oldV, oldC := buildinfo.Version, buildinfo.Commit
	t.Cleanup(func() { buildinfo.Version, buildinfo.Commit = oldV, oldC })
	buildinfo.Version, buildinfo.Commit = version, commit
}

type recordedRequest struct {
	path          string
	authorization []string
}

// fakeHub answers GET /version with its own build and records each request.
type fakeHub struct {
	*httptest.Server
	body map[string]any
	mu   sync.Mutex
	reqs []recordedRequest
}

func newFakeHub(t *testing.T, version string) *fakeHub {
	t.Helper()
	h := &fakeHub{body: map[string]any{
		"version": version, "commit": "c-" + version, "build_time": "2026-01-02T03:04:05Z", "go_version": "go1.27.0",
	}}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		h.reqs = append(h.reqs, recordedRequest{path: r.URL.Path, authorization: r.Header.Values("Authorization")})
		h.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(h.body)
	}))
	t.Cleanup(h.Close)
	return h
}

func (h *fakeHub) requests() []recordedRequest {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]recordedRequest(nil), h.reqs...)
}

func TestVersion_PrintsClientAndHubBuilds(t *testing.T) {
	a := newFakeHub(t, "1.0.0-a")
	b := newFakeHub(t, "2.0.0-b")
	unset := "\x00unset"
	tests := []struct {
		name      string
		flag, env string
		want      *fakeHub
	}{
		{"flag only", a.URL, unset, a},
		{"env only", unset, a.URL, a},
		{"flag wins over env", a.URL, b.URL, a},
		{"empty flag falls back to env", "", b.URL, b},
		{"trailing slash is dropped", a.URL + "/", unset, a},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			before := len(tt.want.requests())
			t.Setenv("PARLEY_ENDPOINT_URL", "")
			if tt.env != unset {
				t.Setenv("PARLEY_ENDPOINT_URL", tt.env)
			}
			args := []string{"version"}
			if tt.flag != unset {
				args = append(args, "--endpoint-url", tt.flag)
			}
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), args, &stdout, &stderr); code != 0 {
				t.Fatalf("Run returned %d, want 0; stderr: %s", code, stderr.String())
			}
			var doc map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
				t.Fatalf("stdout %q is not JSON: %v", stdout.String(), err)
			}
			if len(doc) != 2 || doc["cli"] == nil || doc["server"] == nil {
				t.Fatalf("document members = %v, want cli and server", doc)
			}
			cliDoc, _ := doc["cli"].(map[string]any)
			if _, ok := cliDoc["version"]; !ok || len(cliDoc) != 2 {
				t.Errorf("cli = %v, want exactly version and commit", cliDoc)
			}
			if _, ok := cliDoc["commit"]; !ok {
				t.Errorf("cli = %v, want exactly version and commit", cliDoc)
			}
			if !reflect.DeepEqual(doc["server"], tt.want.body) {
				t.Errorf("server = %v, want %v", doc["server"], tt.want.body)
			}
			reqs := tt.want.requests()[before:]
			if len(reqs) != 1 || reqs[0].path != "/version" || len(reqs[0].authorization) != 0 {
				t.Errorf("hub saw %+v, want one GET /version without Authorization", reqs)
			}
			var buf bytes.Buffer
			if err := json.Indent(&buf, bytes.TrimSpace(stdout.Bytes()), "", "  "); err != nil {
				t.Fatal(err)
			}
			if stdout.String() != buf.String()+"\n" {
				t.Errorf("stdout is not one document indented by 2 spaces and a newline: %q", stdout.String())
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestVersion_WithoutAHubURLPrintsTheClientAlone(t *testing.T) {
	setBuild(t, "1.4.0", "3f2a1bc")
	tests := []struct {
		name   string
		args   []string
		env    string
		setEnv bool
	}{
		{"no flag, env unset", []string{"version"}, "", false},
		{"no flag, env empty", []string{"version"}, "", true},
		{"empty flag, env unset", []string{"version", "--endpoint-url", ""}, "", false},
	}
	const want = "{\n  \"cli\": {\n    \"version\": \"1.4.0\",\n    \"commit\": \"3f2a1bc\"\n  }\n}\n"
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PARLEY_ENDPOINT_URL", tt.env)
			var stdout, stderr bytes.Buffer
			if code := Run(context.Background(), tt.args, &stdout, &stderr); code != 0 {
				t.Fatalf("Run returned %d, want 0", code)
			}
			if stdout.String() != want {
				t.Errorf("stdout = %q, want %q", stdout.String(), want)
			}
			if stderr.Len() != 0 {
				t.Errorf("stderr = %q, want empty", stderr.String())
			}
		})
	}
}

func TestVersion_WarnsAndOmitsTheServerWhenTheHubCannotBeRead(t *testing.T) {
	if versionTimeout != 5*time.Second {
		t.Errorf("default version timeout = %v, want 5 s", versionTimeout)
	}
	closed, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	closedURL := "http://" + closed.Addr().String()
	closed.Close()

	answering := func(status int, body string, delay time.Duration) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
			w.WriteHeader(status)
			w.Write([]byte(body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	tests := []struct {
		name    string
		hub     string
		timeout time.Duration
	}{
		{"connection refused", closedURL, 0},
		{"slower than the timeout", answering(200, `{"version":"x"}`, time.Second), 100 * time.Millisecond},
		{"500 with a JSON body", answering(500, `{"error":"boom"}`, 0), 0},
		{"404", answering(404, `not found`, 0), 0},
		{"200 with a body that is not JSON", answering(200, `not json`, 0), 0},
		{"200 with a JSON array", answering(200, `[]`, 0), 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PARLEY_ENDPOINT_URL", "")
			if tt.timeout > 0 {
				old := versionTimeout
				versionTimeout = tt.timeout
				t.Cleanup(func() { versionTimeout = old })
			}
			var stdout, stderr bytes.Buffer
			start := time.Now()
			code := Run(context.Background(), []string{"version", "--endpoint-url", tt.hub}, &stdout, &stderr)
			if code != 0 {
				t.Fatalf("Run returned %d, want 0", code)
			}
			if elapsed := time.Since(start); elapsed > 6*time.Second {
				t.Errorf("the command took %v, want at most 6 s", elapsed)
			}
			var doc map[string]any
			if err := json.Unmarshal(stdout.Bytes(), &doc); err != nil {
				t.Fatalf("stdout %q is not JSON: %v", stdout.String(), err)
			}
			if _, ok := doc["cli"]; !ok || len(doc) != 1 {
				t.Errorf("document members = %v, want cli alone", doc)
			}
			lines := strings.Split(strings.TrimRight(stderr.String(), "\n"), "\n")
			if len(lines) != 1 || !strings.HasPrefix(lines[0], "warning: ") || !strings.Contains(lines[0], tt.hub) {
				t.Errorf("stderr = %q, want one warning line naming %s", stderr.String(), tt.hub)
			}
		})
	}
}
