package forge_test

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/buildinfo"
	"github.com/agent-fox-dev/parley/internal/forge"
	"github.com/agent-fox-dev/parley/internal/forge/forgetest"
	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/workspace"
)

// fakeClock is a clock whose sleep is recorded and returns at once.
type fakeClock struct {
	mu    sync.Mutex
	now   time.Time
	slept []time.Duration
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Sleep(_ context.Context, d time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.slept = append(c.slept, d)
	return nil
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func (c *fakeClock) sleeps() []time.Duration {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.slept)
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func get(url string) forge.Request {
	return forge.Request{Method: http.MethodGet, URL: url, Kind: forge.KindGitHub,
		Credential: forge.Credential{Kind: forge.CredentialPAT, Token: "t1"}}
}

func header(name, value string) http.Header { return http.Header{name: {value}} }

func TestClient_SendsItsVersionAndTheCredentialsToken(t *testing.T) {
	t.Parallel()
	fake := forgetest.New(t)
	client := forge.NewClient(forge.ClientOptions{Transport: fake.Transport()})
	cases := []struct {
		credential forge.Credential
		kind       forge.ForgeKind
		wantAuth   string
		wantAccept string
	}{
		{forge.Credential{Kind: forge.CredentialPAT, Token: "t1"}, forge.KindGitHub, "Bearer t1", "application/vnd.github+json"},
		{forge.Credential{Kind: forge.CredentialPAT, Token: "t1"}, forge.KindGitLab, "Bearer t1", ""},
		{forge.Credential{Kind: forge.CredentialBasic, Token: "p1"}, forge.KindGitHub, "Bearer p1", "application/vnd.github+json"},
		{forge.Credential{Kind: forge.CredentialBasic, Token: "p1"}, forge.KindGitLab, "Bearer p1", ""},
		{forge.Credential{Kind: forge.CredentialNone}, forge.KindGitHub, "", "application/vnd.github+json"},
		{forge.Credential{Kind: forge.CredentialNone}, forge.KindGitLab, "", ""},
	}
	for _, c := range cases {
		fake.Script(forgetest.Answer{})
		_, err := client.Do(t.Context(), forge.Request{Method: http.MethodGet, URL: fake.URL + "/x",
			Kind: c.kind, Credential: c.credential})
		if err != nil {
			t.Fatalf("%s %s: Do: %v", c.credential.Kind, c.kind, err)
		}
		got := fake.Last(t).Header
		if ua := got.Get("User-Agent"); ua != "parleyd/"+buildinfo.Get().Version {
			t.Errorf("%s %s: User-Agent = %q, want parleyd/%s", c.credential.Kind, c.kind, ua, buildinfo.Get().Version)
		}
		if auth := got.Values("Authorization"); strings.Join(auth, ",") != c.wantAuth {
			t.Errorf("%s %s: Authorization = %q, want %q", c.credential.Kind, c.kind, auth, c.wantAuth)
		}
		if accept := got.Get("Accept"); accept != c.wantAccept {
			t.Errorf("%s %s: Accept = %q, want %q", c.credential.Kind, c.kind, accept, c.wantAccept)
		}
		if got.Get("Private-Token") != "" {
			t.Errorf("%s %s: sent a PRIVATE-TOKEN header", c.credential.Kind, c.kind)
		}
	}
}

// The proxy variables are read from the process environment through
// os.LookupEnv, the environment a hub is given, so this test sets them and
// does not run in parallel.
func TestClient_SendsThroughTheProxyTheHubsEnvironmentNames(t *testing.T) {
	proxy := forgetest.NewPlain(t)
	proxyAddr := strings.TrimPrefix(proxy.URL, "http://")
	cases := []struct {
		name       string
		env        map[string]string
		url        string
		wantDial   string
		wantMethod string // of the request the proxy receives; "" for none
		wantTarget string // host and path the proxy is asked for
	}{
		{"HTTP_PROXY carries a plain request", map[string]string{"HTTP_PROXY": proxy.URL},
			"http://forge.test/x", proxyAddr, http.MethodGet, "forge.test/x"},
		{"HTTPS_PROXY tunnels a TLS request", map[string]string{"HTTPS_PROXY": proxy.URL},
			"https://forge.test/x", proxyAddr, http.MethodConnect, "forge.test:443"},
		{"a host NO_PROXY lists is contacted directly", map[string]string{"HTTP_PROXY": proxy.URL, "NO_PROXY": "forge.test"},
			"http://forge.test/x", "forge.test:80", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, name := range []string{"HTTP_PROXY", "HTTPS_PROXY", "NO_PROXY", "http_proxy", "https_proxy", "no_proxy"} {
				t.Setenv(name, c.env[name])
			}
			var mu sync.Mutex
			var dialed []string
			transport := &http.Transport{DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				mu.Lock()
				dialed = append(dialed, addr)
				mu.Unlock()
				if addr == proxyAddr {
					return (&net.Dialer{}).DialContext(ctx, network, addr)
				}
				return nil, errors.New("no route to " + addr)
			}}
			before := proxy.Count()
			client := forge.NewClient(forge.ClientOptions{Transport: transport, Env: os.LookupEnv})

			client.Do(t.Context(), get(c.url))

			mu.Lock()
			defer mu.Unlock()
			if !slices.Equal(dialed, []string{c.wantDial}) {
				t.Errorf("dialled %q, want %q", dialed, c.wantDial)
			}
			received := proxy.Requests()[before:]
			if c.wantMethod == "" {
				if len(received) != 0 {
					t.Errorf("the proxy received %d requests, want none", len(received))
				}
				return
			}
			if len(received) != 1 || received[0].Method != c.wantMethod || received[0].Host+received[0].Path != c.wantTarget {
				t.Errorf("the proxy received %+v, want one %s for %s", received, c.wantMethod, c.wantTarget)
			}
		})
	}
}

func TestClient_FollowsSameOriginRedirectsUpToTen(t *testing.T) {
	t.Parallel()
	for _, redirects := range []int{1, 10} {
		fake := forgetest.New(t)
		for range redirects {
			fake.Script(forgetest.Answer{Redirect: "/next"})
		}
		fake.Script(forgetest.Answer{Body: `{"final":true}`})
		client := forge.NewClient(forge.ClientOptions{Transport: fake.Transport()})

		resp, err := client.Do(t.Context(), get(fake.URL+"/start"))

		if err != nil {
			t.Fatalf("%d redirects: Do: %v", redirects, err)
		}
		if resp.Status != http.StatusOK || string(resp.Body) != `{"final":true}` {
			t.Errorf("%d redirects: answer %d %s, want 200 with the final body", redirects, resp.Status, resp.Body)
		}
		requests := fake.Requests()
		if len(requests) != redirects+1 {
			t.Errorf("%d redirects: the forge received %d requests, want %d", redirects, len(requests), redirects+1)
		}
		for i, r := range requests {
			if r.Header.Get("Authorization") != "Bearer t1" {
				t.Errorf("%d redirects: request %d carried Authorization %q", redirects, i, r.Header.Get("Authorization"))
			}
		}
	}
}

func TestClient_RetriesOnceAfterTheStatedWaitOfARateLimit(t *testing.T) {
	t.Parallel()
	reset := func(after int64) string { return strconv.FormatInt(newFakeClock().Now().Unix()+after, 10) }
	cases := []struct {
		name  string
		first forgetest.Answer
		wait  time.Duration
	}{
		{"429 with Retry-After", forgetest.Answer{Status: 429, Header: header("Retry-After", "2")}, 2 * time.Second},
		{"403 with Retry-After", forgetest.Answer{Status: 403, Header: header("Retry-After", "4")}, 4 * time.Second},
		{"403 with no requests remaining and X-RateLimit-Reset", forgetest.Answer{Status: 403,
			Header: http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Reset": {reset(3)}}}, 3 * time.Second},
		{"429 with RateLimit-Reset", forgetest.Answer{Status: 429, Header: header("RateLimit-Reset", reset(1))}, time.Second},
		{"429 with a wait of the whole deadline", forgetest.Answer{Status: 429, Header: header("Retry-After", "30")}, 30 * time.Second},
		{"429 with Retry-After 0", forgetest.Answer{Status: 429, Header: header("Retry-After", "0")}, 0},
	}
	for _, c := range cases {
		clock := newFakeClock()
		fake := forgetest.New(t)
		fake.Script(c.first, forgetest.Answer{Body: `{"second":true}`})
		client := forge.NewClient(forge.ClientOptions{Transport: fake.Transport(), Now: clock.Now, Sleep: clock.Sleep})

		resp, err := client.Do(t.Context(), get(fake.URL+"/x"))

		if err != nil {
			t.Fatalf("%s: Do: %v", c.name, err)
		}
		if resp.Status != http.StatusOK || string(resp.Body) != `{"second":true}` {
			t.Errorf("%s: answer %d %s, want the second request's 200", c.name, resp.Status, resp.Body)
		}
		if n := fake.Count(); n != 2 {
			t.Errorf("%s: the forge received %d requests, want 2", c.name, n)
		}
		if got := clock.sleeps(); !slices.Equal(got, []time.Duration{c.wait}) {
			t.Errorf("%s: slept %v, want %v", c.name, got, c.wait)
		}
	}
}

func TestClient_SendsARequestItsCallerHasLeft(t *testing.T) {
	t.Parallel()
	fake := forgetest.New(t)
	fake.Script(forgetest.Answer{Delay: 20 * time.Millisecond})
	client := forge.NewClient(forge.ClientOptions{Transport: fake.Transport()})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	resp, err := client.Do(ctx, get(fake.URL+"/x"))

	if err != nil || resp.Status != http.StatusOK {
		t.Fatalf("Do = %v, %v, want the forge's 200", resp, err)
	}
}

// The forge tokens and API URLs other tools read from a process's
// environment, which the hub reads in neither its own nor the process's.
var forgeEnvironment = map[string]string{
	"GIT_PAT": "envtoken", "UPSTREAM_GIT_PAT": "envtoken",
	"GITHUB_TOKEN": "envtoken", "GH_TOKEN": "envtoken", "GITLAB_TOKEN": "envtoken",
	"GITHUB_API_URL": "https://elsewhere.invalid", "GITLAB_API_URL": "https://elsewhere.invalid",
}

// The variables are set in the process environment, so this test does not
// run in parallel.
func TestForgeRoute_ReadsAnonymouslyWithoutSecretsWhateverTheEnvironmentHolds(t *testing.T) {
	for name, value := range forgeEnvironment {
		t.Setenv(name, value)
	}
	w := newWorldWith(t, hubtest.Config{Env: forgeEnvironment}, nil)
	for slug, apiURL := range map[string]string{"gh": w.fake.GitHubURL(), "gl": w.fake.GitLabURL()} {
		w.addWorkspace(t, workspace.Workspace{Slug: slug, GitURL: new("https://github.com/acme/widgets.git"),
			Settings: settings(forge.SettingAPIURL, apiURL)})
		w.fake.Script(forgetest.Answer{Body: `{"number":1}`}, forgetest.Answer{Body: "[]"})
		if status, body, sent := w.get(t, slug, issuePath, w.Token); status != http.StatusOK || sent != 2 {
			t.Errorf("%s: %d %s after %d forge requests, want 200 after 2, the issue and its comments", slug, status, body, sent)
		}
	}
	for _, r := range w.fake.Requests() {
		if r.Header.Get("Authorization") != "" || r.Header.Get("Private-Token") != "" {
			t.Errorf("%s %s carried credentials: %v", r.Method, r.Path, r.Header)
		}
		for name, values := range r.Header {
			if strings.Contains(strings.Join(values, " "), "envtoken") {
				t.Errorf("%s %s carried the environment's token in %s", r.Method, r.Path, name)
			}
		}
		if strings.Contains(r.RawPath+"?"+r.RawQuery, "envtoken") {
			t.Errorf("%s %s?%s carried the environment's token", r.Method, r.RawPath, r.RawQuery)
		}
	}
}

func TestForgeRoute_AbandonsAForgeThatDoesNotAnswerInTime(t *testing.T) {
	t.Parallel()
	w := newWorldWith(t, hubtest.Config{}, func(_ *world, d *forge.Deps) { d.Client.Deadline = 50 * time.Millisecond })
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	w.fake.Script(forgetest.Answer{Hold: true})
	start := time.Now()

	status, body, _ := w.get(t, "w", issuePath, w.Token)

	if status != http.StatusGatewayTimeout || errorOf(t, body).Type != forge.ErrorTypeTimeout {
		t.Errorf("a forge that holds its answer: %d %s, want 504 %s", status, body, forge.ErrorTypeTimeout)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("the read took %v", elapsed)
	}
}

func TestForgeRoute_FailsARateLimitItCannotWaitFor(t *testing.T) {
	t.Parallel()
	clock := newFakeClock()
	var used atomic.Int64 // the clock's advance while a request is sent
	w := newWorldWith(t, hubtest.Config{}, func(w *world, d *forge.Deps) {
		d.Client.Now, d.Client.Sleep = clock.Now, clock.Sleep
		d.Client.Transport = roundTripFunc(func(r *http.Request) (*http.Response, error) {
			clock.advance(time.Duration(used.Load()))
			return w.fake.Transport().RoundTrip(r)
		})
	})
	// The rows' waits are measured against the deadline of the hub's own
	// client.
	if d := w.forge.Client().Deadline(); d != 30*time.Second {
		t.Fatalf("the hub's forge client gives a request %v, want 30s", d)
	}
	w.addWorkspace(t, workspace.Workspace{Slug: "w", GitURL: new("https://github.com/acme/widgets.git"), Settings: w.github()})
	limited := func(h http.Header) forgetest.Answer { return forgetest.Answer{Status: 429, Header: h} }
	reset := func(after int64) string { return strconv.FormatInt(clock.Now().Unix()+after, 10) }
	cases := []struct {
		name           string
		answers        func() []forgetest.Answer
		used           time.Duration
		wantRequests   int
		wantRetryAfter string
	}{
		{"a wait past the deadline", func() []forgetest.Answer { return []forgetest.Answer{limited(header("Retry-After", "31"))} },
			0, 1, "31"},
		{"no stated wait", func() []forgetest.Answer { return []forgetest.Answer{limited(nil)} }, 0, 1, ""},
		{"403 with no requests remaining and no reset", func() []forgetest.Answer {
			return []forgetest.Answer{{Status: 403, Header: header("X-RateLimit-Remaining", "0")}}
		}, 0, 1, ""},
		{"a retry limited too", func() []forgetest.Answer {
			return []forgetest.Answer{limited(header("Retry-After", "1")), limited(header("Retry-After", "5"))}
		}, 0, 2, "5"},
		{"a wait past what the request left of its deadline", func() []forgetest.Answer {
			return []forgetest.Answer{limited(header("Retry-After", "20"))}
		}, 15 * time.Second, 1, "20"},
		{"a reset's wait rounded up to whole seconds", func() []forgetest.Answer {
			return []forgetest.Answer{limited(header("X-RateLimit-Reset", reset(45)))}
		}, 500 * time.Millisecond, 1, "45"},
	}
	for _, c := range cases {
		used.Store(int64(c.used))
		w.fake.Script(c.answers()...)
		before := w.fake.Count()

		status, h, body := w.Exchange(t, http.MethodGet, "/api/v1/workspaces/w"+issuePath, w.Token, nil, nil)

		if status != http.StatusTooManyRequests || errorOf(t, body).Type != forge.ErrorTypeRateLimited {
			t.Errorf("%s: %d %s, want 429 %s", c.name, status, body, forge.ErrorTypeRateLimited)
		}
		if n := w.fake.Count() - before; n != c.wantRequests {
			t.Errorf("%s: the forge received %d requests, want %d", c.name, n, c.wantRequests)
		}
		if got, ok := h["Retry-After"]; strings.Join(got, ",") != c.wantRetryAfter || ok != (c.wantRetryAfter != "") {
			t.Errorf("%s: Retry-After %q, want %q", c.name, got, c.wantRetryAfter)
		}
	}
}
