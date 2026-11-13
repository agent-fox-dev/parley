package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// The tests in this file set environment variables, so they must not run in
// parallel.

func TestLogin_StoresTheGivenCredentialAndPrintsTheUser(t *testing.T) {
	var indented bytes.Buffer
	json.Indent(&indented, []byte(testUser), "", "  ")
	wantStdout := indented.String() + "\n"

	for _, cred := range []string{testAPIKey, testPAT} {
		for _, extra := range [][]string{nil, {"--provider", "google", "--expires", "30"}} {
			t.Run(cred[:11]+" "+strings.Join(extra, " "), func(t *testing.T) {
				dir := loginEnv(t)
				hub := userHub(t, testUser)

				code, stdout, stderr := runCLI(t, append([]string{"login", "--api-key", cred, "--endpoint-url", hub.URL}, extra...)...)

				if code != 0 {
					t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
				}
				if stdout != wantStdout {
					t.Errorf("stdout = %q, want the User indented by 2 spaces and a newline, %q", stdout, wantStdout)
				}
				if !slices.Contains(nonEmptyLines(stderr), "Logged in as alice") {
					t.Errorf("stderr = %q, want the line %q", stderr, "Logged in as alice")
				}
				reqs := hub.recorded()
				if len(reqs) != 1 {
					t.Fatalf("the hub got %d requests, want 1", len(reqs))
				}
				if q := reqs[0]; q.method != http.MethodGet || q.path != "/api/v1/user" || q.header.Get("Authorization") != "Bearer "+cred {
					t.Errorf("request = %s %s with Authorization %q, want GET /api/v1/user with Bearer %s", q.method, q.path, q.header.Get("Authorization"), cred)
				}
				if got := readTOML(t, filepath.Join(dir, "config.toml"))["api_key"]; got != cred {
					t.Errorf("saved api_key = %v, want %s", got, cred)
				}
			})
		}
	}
}

func TestLogin_ReadsTheCredentialFromStdin(t *testing.T) {
	for _, stdin := range []string{testAPIKey + "\n", testAPIKey, "  " + testAPIKey + "\r\n"} {
		t.Run(stdin, func(t *testing.T) {
			dir := loginEnv(t)
			hub := userHub(t, testUser)

			code, _, stderr := runWithStdin(t, stdin, "login", "--api-key", "-", "--endpoint-url", hub.URL)

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			if reqs := hub.recorded(); len(reqs) == 0 || reqs[0].header.Get("Authorization") != "Bearer "+testAPIKey {
				t.Errorf("requests = %v, want the first with Authorization Bearer %s", reqs, testAPIKey)
			}
			if got := readTOML(t, filepath.Join(dir, "config.toml"))["api_key"]; got != testAPIKey {
				t.Errorf("saved api_key = %v, want %s", got, testAPIKey)
			}
		})
	}
}

func TestLogin_PrintsTheHubsErrorAndSavesNothing(t *testing.T) {
	answers := []struct {
		name   string
		status int
		body   string
	}{
		{"invalid credentials", 401, `{"error":{"code":401,"message":"invalid credentials","error_type":"unauthenticated"}}` + "\n"},
		{"the admin token", 403, `{"error":{"code":403,"message":"this route needs a user","error_type":"forbidden"}}` + "\n"},
		{"a PAT without users:read", 403, `{"error":{"code":403,"message":"the token lacks the scope users:read","error_type":"insufficient_scope"}}` + "\n"},
	}
	for _, a := range answers {
		for _, existing := range []bool{false, true} {
			t.Run(a.name+map[bool]string{false: "", true: ", existing file"}[existing], func(t *testing.T) {
				dir := loginEnv(t)
				path := filepath.Join(dir, "config.toml")
				before := []byte("endpoint_url = \"http://old\"\napi_key = \"old\"\n")
				if existing {
					os.MkdirAll(dir, 0o700)
					os.WriteFile(path, before, 0o600)
				}
				hub := newStubHub(t).answer(http.MethodGet, "/api/v1/user", a.status, a.body)

				code, stdout, stderr := runCLI(t, "login", "--api-key", testAPIKey, "--endpoint-url", hub.URL)

				if code != 1 {
					t.Errorf("exit status = %d, want 1", code)
				}
				if stderr != a.body {
					t.Errorf("stderr = %q, want the hub's envelope as received, %q", stderr, a.body)
				}
				if stdout != "" {
					t.Errorf("stdout = %q, want it empty", stdout)
				}
				if !existing {
					wantAbsent(t, path)
				} else if got, _ := os.ReadFile(path); !bytes.Equal(got, before) {
					t.Errorf("config file = %q, want its bytes unchanged, %q", got, before)
				}
			})
		}
	}
}

func TestLogin_FailsOnTheClientSideWhenItCannotReadAnAnswer(t *testing.T) {
	tests := []struct {
		name string
		// answer is the stub's answer to GET /api/v1/user; endpoint, when
		// set, replaces the stub's URL.
		answer   stubAnswer
		endpoint string
		apiKey   string
		stdin    string
		want     string
	}{
		{name: "unreachable hub", endpoint: "http://127.0.0.1:1", apiKey: testAPIKey, want: "client_error"},
		{name: "200 that is not JSON", answer: stubAnswer{200, "hello"}, apiKey: testAPIKey, want: "client_error"},
		{name: "200 with a JSON array", answer: stubAnswer{200, "[]"}, apiKey: testAPIKey, want: "client_error"},
		{name: "502 without an envelope", answer: stubAnswer{502, "<html>bad gateway</html>"}, apiKey: testAPIKey, want: "client_error"},
		{name: "empty credential", answer: stubAnswer{200, testUser}, apiKey: "", want: "usage_error"},
		{name: "empty stdin", answer: stubAnswer{200, testUser}, apiKey: "-", stdin: "", want: "usage_error"},
		{name: "blank stdin", answer: stubAnswer{200, testUser}, apiKey: "-", stdin: " \n", want: "usage_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := loginEnv(t)
			hub := newStubHub(t).answer(http.MethodGet, "/api/v1/user", tt.answer.status, tt.answer.body)
			endpoint := hub.URL
			if tt.endpoint != "" {
				endpoint = tt.endpoint
			}

			code, stdout, stderr := runWithStdin(t, tt.stdin, "login", "--api-key", tt.apiKey, "--endpoint-url", endpoint)

			wantClientFailure(t, code, stdout, stderr, tt.want)
			wantAbsent(t, dir)
			if n := len(hub.recorded()); tt.want == "usage_error" && n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
		})
	}
}

func TestLogin_TakesTheHubURLFromTheFlagTheEnvironmentOrTheFile(t *testing.T) {
	tests := []struct {
		name string
		// flag and env are "" when absent; the file always names hub C.
		flag, env, want string
	}{
		{name: "flag first", flag: "A", env: "B", want: "A"},
		{name: "environment without a flag", env: "B", want: "B"},
		{name: "file without flag or environment", want: "C"},
		{name: "trailing slashes dropped", flag: "A///", want: "A"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := loginEnv(t)
			hubs := map[string]*stubHub{"A": userHub(t, testUser), "B": userHub(t, testUser), "C": userHub(t, testUser)}
			resolve := strings.NewReplacer("A", hubs["A"].URL, "B", hubs["B"].URL).Replace
			os.MkdirAll(dir, 0o700)
			os.WriteFile(filepath.Join(dir, "config.toml"), []byte("endpoint_url = \""+hubs["C"].URL+"\"\n"), 0o600)
			args := []string{"login", "--api-key", testAPIKey}
			if tt.flag != "" {
				args = append(args, "--endpoint-url", resolve(tt.flag))
			}
			t.Setenv("PARLEY_ENDPOINT_URL", resolve(tt.env))

			code, _, stderr := runCLI(t, args...)

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			for name, hub := range hubs {
				reqs := hub.recorded()
				switch {
				case name != tt.want && len(reqs) != 0:
					t.Errorf("hub %s got %d requests, want none", name, len(reqs))
				case name == tt.want && (len(reqs) != 1 || reqs[0].path != "/api/v1/user"):
					t.Errorf("hub %s got %v, want one request to /api/v1/user", name, reqs)
				}
			}
		})
	}
}

func TestLogin_FailsWithoutAHubURL(t *testing.T) {
	for _, args := range [][]string{{"login"}, {"login", "--api-key", testAPIKey}} {
		t.Run(strings.Join(args, " "), func(t *testing.T) {
			dir := loginEnv(t)

			code, stdout, stderr := runCLI(t, args...)

			message := wantClientFailure(t, code, stdout, stderr, "client_error")
			for _, name := range []string{"--endpoint-url", "PARLEY_ENDPOINT_URL", "endpoint_url"} {
				if !strings.Contains(message, name) {
					t.Errorf("message %q does not name %s", message, name)
				}
			}
			wantAbsent(t, dir)
		})
	}
}
