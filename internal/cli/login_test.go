package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/BurntSushi/toml"
)

type stubRequest struct {
	method string
	path   string
	header http.Header
	body   string
}

type stubAnswer struct {
	status int
	body   string
}

// stubHub answers the hub's login routes from a script and records every
// request.
type stubHub struct {
	*httptest.Server
	mu      sync.Mutex
	reqs    []stubRequest
	answers map[string]stubAnswer // "METHOD /path"
}

func newStubHub(t *testing.T) *stubHub {
	t.Helper()
	s := &stubHub{answers: map[string]stubAnswer{
		"GET /api/v1/user": {200, `{"id":"u-1","username":"alice","email":"a@x"}`},
	}}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		s.mu.Lock()
		s.reqs = append(s.reqs, stubRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), body: string(b)})
		a, ok := s.answers[r.Method+" "+r.URL.Path]
		s.mu.Unlock()
		if !ok {
			w.WriteHeader(404)
			io.WriteString(w, `{"error":{"code":404,"message":"not found","error_type":"not_found"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(a.status)
		io.WriteString(w, a.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *stubHub) answer(route string, status int, body string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers[route] = stubAnswer{status, body}
}

func (s *stubHub) requests() []stubRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]stubRequest(nil), s.reqs...)
}

// cliEnv points the config dir at a fresh directory and clears the hub URL
// and credential variables; it returns the config dir.
func cliEnv(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("PARLEY_CONFIG_DIR", dir)
	t.Setenv("PARLEY_ENDPOINT_URL", "")
	t.Setenv("PARLEY_API_KEY", "")
	return dir
}

type cliResult struct {
	code   int
	stdout string
	stderr string
}

func runCLI(t *testing.T, stdin string, args ...string) cliResult {
	t.Helper()
	var out, errb bytes.Buffer
	code := RunWith(context.Background(), args, Options{Stdin: strings.NewReader(stdin)}, &out, &errb)
	return cliResult{code, out.String(), errb.String()}
}

// envelope parses the error envelope on stderr (the last JSON document).
func (r cliResult) envelope(t *testing.T) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(r.stderr), "\n")
	var doc struct {
		Error map[string]any `json:"error"`
	}
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &doc); err != nil || doc.Error == nil {
		t.Fatalf("stderr %q is not an error envelope", r.stderr)
	}
	return doc.Error
}

func readTOML(t *testing.T, path string) map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := toml.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

const apiKey = "parley_abcd1234_ABCDEFGHIJKLMNOPQRSTUVWXYZ012345"

func TestLogin_TheHubURLComesFromFlagEnvironmentOrFile(t *testing.T) {
	a, b, c := newStubHub(t), newStubHub(t), newStubHub(t)
	tests := []struct {
		name      string
		flag, env string
		file      string
		want      *stubHub
		withSlash bool
	}{
		{"flag wins", a.URL, b.URL, c.URL, a, false},
		{"environment next", "", b.URL, c.URL, b, false},
		{"file last", "", "", c.URL, c, false},
		{"flag with trailing slashes", a.URL, "", "", a, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := cliEnv(t)
			t.Setenv("PARLEY_ENDPOINT_URL", tt.env)
			if tt.file != "" {
				os.MkdirAll(dir, 0o700)
				os.WriteFile(filepath.Join(dir, "config.toml"), []byte("endpoint_url = \""+tt.file+"\"\n"), 0o600)
			}
			before := map[*stubHub]int{a: len(a.requests()), b: len(b.requests()), c: len(c.requests())}
			args := []string{"login", "--api-key", apiKey}
			if tt.flag != "" {
				f := tt.flag
				if tt.withSlash {
					f += "///"
				}
				args = append(args, "--endpoint-url", f)
			}
			if r := runCLI(t, "", args...); r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			for _, hub := range []*stubHub{a, b, c} {
				got := hub.requests()[before[hub]:]
				if hub == tt.want {
					if len(got) != 1 || got[0].path != "/api/v1/user" {
						t.Errorf("wanted hub got %+v", got)
					}
				} else if len(got) != 0 {
					t.Errorf("another hub got %d requests", len(got))
				}
			}
		})
	}
}

func TestLogin_WithoutAHubURLFailsBeforeAnyRequest(t *testing.T) {
	for _, args := range [][]string{{"login"}, {"login", "--api-key", apiKey}} {
		dir := cliEnv(t)
		r := runCLI(t, "", args...)
		if r.code != 2 {
			t.Fatalf("%v: exit %d", args, r.code)
		}
		e := r.envelope(t)
		if e["code"] != float64(0) || e["error_type"] != "client_error" {
			t.Errorf("%v: %v", args, e)
		}
		for _, want := range []string{"--endpoint-url", "PARLEY_ENDPOINT_URL", "endpoint_url"} {
			if !strings.Contains(e["message"].(string), want) {
				t.Errorf("message %q lacks %s", e["message"], want)
			}
		}
		if r.stdout != "" {
			t.Errorf("stdout %q", r.stdout)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Error("the config dir was created")
		}
	}
}

func TestLogin_AKeyIsStoredAfterTheHubAcceptsIt(t *testing.T) {
	user := `{"id":"u-1","username":"alice","email":"a@x"}`
	pat := "parley_pat_abcd1234_" + strings.Repeat("a", 32)
	for _, cred := range []string{apiKey, pat} {
		for _, extra := range [][]string{nil, {"--provider", "google", "--expires", "30"}} {
			dir := cliEnv(t)
			hub := newStubHub(t)
			hub.answer("GET /api/v1/user", 200, user)
			r := runCLI(t, "", append([]string{"login", "--api-key", cred, "--endpoint-url", hub.URL}, extra...)...)
			if r.code != 0 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			var want bytes.Buffer
			json.Indent(&want, []byte(user), "", "  ")
			if r.stdout != want.String()+"\n" {
				t.Errorf("stdout = %q", r.stdout)
			}
			if !strings.Contains(r.stderr, "Logged in as alice") {
				t.Errorf("stderr = %q", r.stderr)
			}
			reqs := hub.requests()
			if len(reqs) != 1 || reqs[0].method != "GET" || reqs[0].path != "/api/v1/user" || reqs[0].header.Get("Authorization") != "Bearer "+cred {
				t.Errorf("requests %+v", reqs)
			}
			if got := readTOML(t, filepath.Join(dir, "config.toml"))["api_key"]; got != cred {
				t.Errorf("saved api_key %v", got)
			}
		}
	}
}

func TestLogin_TheKeyCanComeFromStdin(t *testing.T) {
	for _, in := range []string{apiKey + "\n", apiKey, "  " + apiKey + "\r\n"} {
		dir := cliEnv(t)
		hub := newStubHub(t)
		r := runCLI(t, in, "login", "--api-key", "-", "--endpoint-url", hub.URL)
		if r.code != 0 {
			t.Fatalf("%q: exit %d %s", in, r.code, r.stderr)
		}
		if got := hub.requests()[0].header.Get("Authorization"); got != "Bearer "+apiKey {
			t.Errorf("%q: Authorization %q", in, got)
		}
		if got := readTOML(t, filepath.Join(dir, "config.toml"))["api_key"]; got != apiKey {
			t.Errorf("%q: saved %v", in, got)
		}
	}
}

func TestLogin_AHubErrorIsPrintedAsReceivedAndNothingIsSaved(t *testing.T) {
	answers := []struct {
		status int
		body   string
	}{
		{401, `{"error":{"code":401,"message":"invalid credentials","error_type":"unauthenticated"}}`},
		{403, `{"error":{"code":403,"message":"this route needs a user","error_type":"forbidden"}}`},
		{403, `{"error":{"code":403,"message":"missing scope users:read","error_type":"insufficient_scope"}}`},
	}
	for _, a := range answers {
		for _, existing := range []bool{false, true} {
			dir := cliEnv(t)
			before := []byte("endpoint_url = \"http://old\"\napi_key = \"old\"\n")
			if existing {
				os.MkdirAll(dir, 0o700)
				os.WriteFile(filepath.Join(dir, "config.toml"), before, 0o600)
			}
			hub := newStubHub(t)
			hub.answer("GET /api/v1/user", a.status, a.body)
			r := runCLI(t, "", "login", "--api-key", apiKey, "--endpoint-url", hub.URL)
			if r.code != 1 || strings.TrimSpace(r.stderr) != a.body || r.stdout != "" {
				t.Errorf("%d: exit %d stderr %q stdout %q", a.status, r.code, r.stderr, r.stdout)
			}
			got, err := os.ReadFile(filepath.Join(dir, "config.toml"))
			if existing && !bytes.Equal(got, before) {
				t.Error("the existing file changed")
			}
			if !existing && err == nil {
				t.Error("a config file was created")
			}
		}
	}
}

func TestLogin_AKeyFlowClientFailureSavesNothing(t *testing.T) {
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedURL := "http://" + closed.Addr().String()
	closed.Close()
	notJSON := newStubHub(t)
	notJSON.answer("GET /api/v1/user", 200, "hello")
	empty := newStubHub(t)
	tests := []struct {
		name     string
		endpoint string
		args     []string
		stdin    string
		want     string
		hub      *stubHub
	}{
		{"unreachable", closedURL, []string{"--api-key", apiKey}, "", "client_error", nil},
		{"not JSON", notJSON.URL, []string{"--api-key", apiKey}, "", "client_error", nil},
		{"empty credential", empty.URL, []string{"--api-key", ""}, "", "usage_error", empty},
		{"empty stdin", empty.URL, []string{"--api-key", "-"}, "", "usage_error", empty},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := cliEnv(t)
			r := runCLI(t, tt.stdin, append([]string{"login", "--endpoint-url", tt.endpoint}, tt.args...)...)
			if r.code != 2 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			e := r.envelope(t)
			if e["code"] != float64(0) || e["error_type"] != tt.want || r.stdout != "" {
				t.Errorf("%v stdout %q", e, r.stdout)
			}
			if _, err := os.Stat(dir); err == nil {
				t.Error("the config dir was created")
			}
			if tt.hub != nil && len(tt.hub.requests()) != 0 {
				t.Error("the hub got a request")
			}
		})
	}
}
