package cli

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"
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

// testWait is how long a browser login of a test waits for the redirect
// when the test does not mean it to time out.
const testWait = 10 * time.Second

var (
	loopbackCallback = regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+/callback$`)
	hexState         = regexp.MustCompile(`^[0-9a-f]{32}$`)
)

func TestLogin_OpensTheProvidersAuthorizeURLWithARedirectToItsListener(t *testing.T) {
	const googleURL = "https://accounts.example/o/auth?client_id=g-id&response_type=code&scope=openid+email+profile"
	tests := []struct {
		name       string
		args       []string
		wantHost   string
		wantParams map[string]string
	}{
		{"github without --provider", nil, "idp.example", map[string]string{"client_id": "gh-id", "scope": "user:email"}},
		{"google with --provider", []string{"--provider", "google"}, "accounts.example", map[string]string{"client_id": "g-id", "response_type": "code", "scope": "openid email profile"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			loginEnv(t)
			hub := browserHub(t).answer(http.MethodGet, "/api/v1/auth/providers", http.StatusOK,
				providersAnswer("github", testAuthorizeURL, "google", googleURL))
			states := map[string]bool{}
			for range 2 {
				b := &fakeBrowser{query: approve}

				code, _, stderr := runLogin(t, b, testWait, append([]string{"--endpoint-url", hub.URL}, tt.args...)...)

				if code != 0 || len(b.urls) != 1 {
					t.Fatalf("exit status %d after %d opener calls, want 0 after 1; stderr %q", code, len(b.urls), stderr)
				}
				u := b.last()
				q := u.Query()
				if u.Host != tt.wantHost {
					t.Errorf("opened host %q, want %q", u.Host, tt.wantHost)
				}
				for k, v := range tt.wantParams {
					if q.Get(k) != v {
						t.Errorf("opened %s = %q, want %q (kept from the authorize_url)", k, q.Get(k), v)
					}
				}
				if !loopbackCallback.MatchString(q.Get("redirect_uri")) || !b.reachable {
					t.Errorf("redirect_uri %q (taking connections: %v), want http://127.0.0.1:<listening port>/callback", q.Get("redirect_uri"), b.reachable)
				}
				if !hexState.MatchString(q.Get("state")) {
					t.Errorf("state %q, want 32 hexadecimal characters", q.Get("state"))
				}
				states[q.Get("state")] = true
			}
			if len(states) != 2 {
				t.Errorf("the two runs sent the states %v, want two different ones", states)
			}
			for _, r := range hub.recorded() {
				if r.path == "/api/v1/auth/providers" && r.header.Get("Authorization") != "" {
					t.Errorf("GET /api/v1/auth/providers carried Authorization %q, want none", r.header.Get("Authorization"))
				}
			}
			if reqs := hub.recorded(); len(reqs) == 0 || reqs[0].path != "/api/v1/auth/providers" {
				t.Errorf("the first request was not GET /api/v1/auth/providers: %v", reqs)
			}
		})
	}
}

func TestLogin_PrintsTheURLWhenNoBrowserOpensAndKeepsWaiting(t *testing.T) {
	dir := loginEnv(t)
	hub := browserHub(t)
	opts := defaultOptions()
	opts.openBrowser, opts.loginWait = (&fakeBrowser{fail: errors.New("no browser")}).open, testWait
	var stdout, stderr syncBuffer
	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), []string{"login", "--endpoint-url", hub.URL}, &stdout, &stderr, opts)
	}()

	var printed string
	for deadline := time.Now().Add(5 * time.Second); printed == "" && time.Now().Before(deadline); time.Sleep(5 * time.Millisecond) {
		for _, line := range nonEmptyLines(stderr.String()) {
			if i := strings.Index(line, "https://idp.example/authorize"); i >= 0 {
				printed = strings.TrimSpace(line[i:])
			}
		}
	}
	u, err := url.Parse(printed)
	if printed == "" || err != nil {
		t.Fatalf("no authorize URL on stderr within 5 s: %q", stderr.String())
	}
	q := u.Query()
	res, err := http.Get(q.Get("redirect_uri") + "?code=c1&state=" + q.Get("state"))
	if err != nil {
		t.Fatalf("the redirect to the printed redirect_uri: %v", err)
	}
	res.Body.Close()

	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the login did not return after the redirect")
	}
	if got := readTOML(t, filepath.Join(dir, "config.toml"))["api_key"]; got != testIssuedKey {
		t.Errorf("saved api_key = %v, want %s", got, testIssuedKey)
	}
}

func TestLogin_SignsInThroughTheBrowserAndSavesTheKey(t *testing.T) {
	var indented bytes.Buffer
	json.Indent(&indented, []byte(testUser), "", "  ")
	wantStdout := indented.String() + "\n"

	for _, tt := range []struct {
		flags []string
		want  float64
	}{{nil, 90}, {[]string{"--expires", "30"}, 30}, {[]string{"--expires", "0"}, 0}} {
		t.Run(strings.Join(tt.flags, " "), func(t *testing.T) {
			dir := loginEnv(t)
			hub := browserHub(t)
			b := &fakeBrowser{query: approve}

			code, stdout, stderr := runLogin(t, b, testWait, append([]string{"--endpoint-url", hub.URL}, tt.flags...)...)

			if code != 0 {
				t.Fatalf("exit status = %d, want 0; stderr %q", code, stderr)
			}
			reqs := hub.recorded()
			if len(reqs) != 2 || reqs[1].method != http.MethodPost || reqs[1].path != "/api/v1/auth/callback" {
				t.Fatalf("requests = %v, want the providers, then POST /api/v1/auth/callback", reqs)
			}
			var body map[string]any
			json.Unmarshal([]byte(reqs[1].body), &body)
			want := map[string]any{"provider": "github", "code": "c1", "redirect_uri": b.redirectURI(), "expires": tt.want}
			if !reflect.DeepEqual(body, want) {
				t.Errorf("callback body = %v, want %v", body, want)
			}
			if a := reqs[1].header.Get("Authorization"); a != "" {
				t.Errorf("the callback carried Authorization %q, want none", a)
			}
			if b.status != http.StatusOK || !strings.HasPrefix(b.contentType, "text/html") {
				t.Errorf("the browser got %d %q, want 200 text/html", b.status, b.contentType)
			}
			if stdout != wantStdout || strings.Contains(stdout, testIssuedKey) {
				t.Errorf("stdout = %q, want the User alone indented by 2 spaces, %q", stdout, wantStdout)
			}
			if !slices.Contains(nonEmptyLines(stderr), "Logged in as alice") {
				t.Errorf("stderr = %q, want the line %q", stderr, "Logged in as alice")
			}
			wantFile := map[string]any{"endpoint_url": hub.URL, "api_key": testIssuedKey, "user_id": "u-1"}
			if got := readTOML(t, filepath.Join(dir, "config.toml")); !reflect.DeepEqual(got, wantFile) {
				t.Errorf("config file = %v, want %v", got, wantFile)
			}
			wantClosed(t, b.redirectURI())
		})
	}
}

func TestLogin_RefusesARedirectWithoutTheStateOrACode(t *testing.T) {
	queries := map[string]func(string) string{
		"another state": func(string) string { return "code=c1&state=wrong" },
		"no state":      func(string) string { return "code=c1" },
		"an error":      func(s string) string { return "error=access_denied&state=" + s },
		"no code":       func(s string) string { return "state=" + s },
	}
	for name, query := range queries {
		t.Run(name, func(t *testing.T) {
			dir := loginEnv(t)
			hub := browserHub(t)
			b := &fakeBrowser{query: query}

			code, stdout, stderr := runLogin(t, b, testWait, "--endpoint-url", hub.URL)

			wantClientFailure(t, code, stdout, stderr, "client_error")
			if b.status != http.StatusBadRequest || !strings.HasPrefix(b.contentType, "text/html") {
				t.Errorf("the browser got %d %q, want 400 text/html", b.status, b.contentType)
			}
			if n := hub.callbacks(); n != 0 {
				t.Errorf("the hub got %d callbacks, want none", n)
			}
			wantAbsent(t, dir)
		})
	}
}

func TestLogin_GivesUpWhenNoRedirectArrives(t *testing.T) {
	dir := loginEnv(t)
	hub := browserHub(t)
	b := &fakeBrowser{}
	start := time.Now()

	code, stdout, stderr := runLogin(t, b, 100*time.Millisecond, "--endpoint-url", hub.URL)

	if took := time.Since(start); took >= 2*time.Second {
		t.Errorf("the login took %v, want it to give up after the 100 ms wait", took)
	}
	wantClientFailure(t, code, stdout, stderr, "client_error")
	wantClosed(t, b.redirectURI())
	if n := hub.callbacks(); n != 0 {
		t.Errorf("the hub got %d callbacks, want none", n)
	}
	wantAbsent(t, dir)
}

func TestLogin_PrintsTheHubsErrorOfABrowserLoginAndSavesNothing(t *testing.T) {
	envelope := func(code int, message, errorType string) string {
		return fmt.Sprintf(`{"error":{"code":%d,"message":%q,"error_type":%q}}`, code, message, errorType) + "\n"
	}
	tests := []struct {
		name, path string
		status     int
		body       string
	}{
		{"providers rate limited", "/api/v1/auth/providers", 429, envelope(429, "too many requests", "rate_limited")},
		{"callback invalid", "/api/v1/auth/callback", 400, envelope(400, "redirect_uri is not allowed", "invalid_request")},
		{"callback rejected", "/api/v1/auth/callback", 401, envelope(401, "the identity provider rejected the code", "unauthenticated")},
		{"callback blocked", "/api/v1/auth/callback", 403, envelope(403, "user is blocked", "forbidden")},
		{"callback conflict", "/api/v1/auth/callback", 409, envelope(409, "the e-mail belongs to another user", "conflict")},
		{"callback upstream", "/api/v1/auth/callback", 502, envelope(502, "the identity provider failed", "upstream_error")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := loginEnv(t)
			method := map[bool]string{true: http.MethodGet, false: http.MethodPost}[strings.HasSuffix(tt.path, "providers")]
			hub := browserHub(t).answer(method, tt.path, tt.status, tt.body)
			b := &fakeBrowser{query: approve}

			code, stdout, stderr := runLogin(t, b, testWait, "--endpoint-url", hub.URL)

			if code != 1 {
				t.Errorf("exit status = %d, want 1", code)
			}
			if !strings.HasSuffix(stderr, tt.body) {
				t.Errorf("stderr = %q, want it to end with the hub's envelope as received, %q", stderr, tt.body)
			}
			if stdout != "" {
				t.Errorf("stdout = %q, want it empty", stdout)
			}
			wantAbsent(t, dir)
			if method == http.MethodGet && len(b.urls) != 0 {
				t.Errorf("the opener was called %d times, want none", len(b.urls))
			}
		})
	}
}

func TestLogin_FailsBeforeOpeningTheBrowser(t *testing.T) {
	tests := []struct {
		name     string
		endpoint string
		// providers, when set, replaces the stub's provider list answer.
		providers *stubAnswer
		args      []string
		want      string
		// mentions is a text the message must hold.
		mentions string
	}{
		{name: "unreachable hub", endpoint: "http://127.0.0.1:1", want: "client_error"},
		{name: "provider list not JSON", providers: &stubAnswer{200, "hello"}, want: "client_error"},
		{name: "provider not listed", args: []string{"--provider", "google"}, want: "client_error", mentions: "github"},
		{name: "no provider listed", providers: &stubAnswer{200, providersAnswer()}, want: "client_error"},
		{name: "--expires 45", args: []string{"--expires", "45"}, want: "usage_error"},
		{name: "--expires x", args: []string{"--expires", "x"}, want: "usage_error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := loginEnv(t)
			hub := browserHub(t)
			if tt.providers != nil {
				hub.answer(http.MethodGet, "/api/v1/auth/providers", tt.providers.status, tt.providers.body)
			}
			endpoint := cmp.Or(tt.endpoint, hub.URL)
			b := &fakeBrowser{query: approve}

			code, stdout, stderr := runLogin(t, b, testWait, append([]string{"--endpoint-url", endpoint}, tt.args...)...)

			message := wantClientFailure(t, code, stdout, stderr, tt.want)
			if !strings.Contains(message, tt.mentions) {
				t.Errorf("message %q does not name %q", message, tt.mentions)
			}
			if len(b.urls) != 0 {
				t.Errorf("the opener was called %d times, want none", len(b.urls))
			}
			if n := len(hub.recorded()); tt.want == "usage_error" && n != 0 {
				t.Errorf("the hub got %d requests, want none", n)
			}
			wantAbsent(t, dir)
		})
	}
}
