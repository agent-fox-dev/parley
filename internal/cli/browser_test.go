package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

const (
	userJSON   = `{"id":"u-1","username":"alice","email":"a@x"}`
	browserKey = "parley_KEYK1234_" + "KKKKKKKKKKKKKKKKKKKKKKKKKKKKKKKK"
)

// browserHub is a stub hub listing github and answering the callback.
func browserHub(t *testing.T) *stubHub {
	t.Helper()
	h := newStubHub(t)
	h.answer("GET /api/v1/auth/providers", 200, `{"items":[{"name":"github","authorize_url":"https://idp.example/authorize?client_id=gh-id&scope=user%3Aemail"}],"next_cursor":null}`)
	h.answer("POST /api/v1/auth/callback", 200, `{"user":`+userJSON+`,"api_key":{"key":"`+browserKey+`","key_id":"KEYK1234","expires_at":null}}`)
	return h
}

// fakeBrowser records the URL it is asked to open and then plays the
// browser: it sends the GET query(state) returns to the redirect_uri.
type fakeBrowser struct {
	mu          sync.Mutex
	urls        []string
	calls       int
	fail        bool
	query       func(state string) string
	status      int
	contentType string
}

func (b *fakeBrowser) open(raw string) error {
	b.mu.Lock()
	b.calls++
	b.urls = append(b.urls, raw)
	b.mu.Unlock()
	if b.fail {
		return errors.New("no browser")
	}
	if b.query == nil {
		return nil
	}
	u, err := url.Parse(raw)
	if err != nil {
		return err
	}
	q := u.Query()
	resp, err := http.Get(q.Get("redirect_uri") + "?" + b.query(q.Get("state")))
	if err != nil {
		return err
	}
	resp.Body.Close()
	b.mu.Lock()
	b.status, b.contentType = resp.StatusCode, resp.Header.Get("Content-Type")
	b.mu.Unlock()
	return nil
}

func (b *fakeBrowser) lastURL(t *testing.T) *url.URL {
	t.Helper()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.urls) == 0 {
		t.Fatal("the browser was not opened")
	}
	u, err := url.Parse(b.urls[len(b.urls)-1])
	if err != nil {
		t.Fatal(err)
	}
	return u
}

func approve(code string) func(string) string {
	return func(state string) string { return "code=" + code + "&state=" + state }
}

func runBrowser(t *testing.T, b *fakeBrowser, wait time.Duration, args ...string) cliResult {
	t.Helper()
	var out, errb bytes.Buffer
	code := RunWith(context.Background(), args, Options{Opener: b.open, LoginWait: wait}, &out, &errb)
	return cliResult{code, out.String(), errb.String()}
}

var stateForm = regexp.MustCompile(`^[0-9a-f]{32}$`)

func TestBrowserLogin_OpensTheAuthorizeURLWithRedirectAndState(t *testing.T) {
	var states []string
	for range 2 {
		cliEnv(t)
		hub := browserHub(t)
		b := &fakeBrowser{query: approve("c1")}
		if r := runBrowser(t, b, time.Second, "login", "--endpoint-url", hub.URL); r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		u := b.lastURL(t)
		q := u.Query()
		if u.Host != "idp.example" || q.Get("client_id") != "gh-id" || q.Get("scope") != "user:email" {
			t.Errorf("url %s", u)
		}
		if !regexp.MustCompile(`^http://127\.0\.0\.1:[0-9]+/callback$`).MatchString(q.Get("redirect_uri")) || !stateForm.MatchString(q.Get("state")) {
			t.Errorf("redirect_uri %q state %q", q.Get("redirect_uri"), q.Get("state"))
		}
		first := hub.requests()[0]
		if first.path != "/api/v1/auth/providers" || first.header.Get("Authorization") != "" {
			t.Errorf("first request %+v", first)
		}
		states = append(states, q.Get("state"))
	}
	if states[0] == states[1] {
		t.Error("two runs used the same state")
	}

	cliEnv(t)
	hub := browserHub(t)
	hub.answer("GET /api/v1/auth/providers", 200, `{"items":[{"name":"github","authorize_url":"https://idp.example/authorize?client_id=gh-id"},{"name":"google","authorize_url":"https://accounts.example/auth?client_id=gg"}],"next_cursor":null}`)
	b := &fakeBrowser{query: approve("c1")}
	if r := runBrowser(t, b, time.Second, "login", "--endpoint-url", hub.URL, "--provider", "google"); r.code != 0 {
		t.Fatalf("google: exit %d: %s", r.code, r.stderr)
	}
	if b.lastURL(t).Host != "accounts.example" {
		t.Errorf("google opened %s", b.lastURL(t))
	}
}

// syncWriter is a buffer the login may write while the test reads it.
type syncWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (w *syncWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.Write(p)
}

func (w *syncWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestBrowserLogin_WithoutABrowserTheURLIsPrinted(t *testing.T) {
	dir := cliEnv(t)
	hub := browserHub(t)
	b := &fakeBrowser{fail: true}
	var out bytes.Buffer
	errw := &syncWriter{}
	done := make(chan int, 1)
	go func() {
		done <- RunWith(context.Background(), []string{"login", "--endpoint-url", hub.URL}, Options{Opener: b.open, LoginWait: 5 * time.Second}, &out, errw)
	}()
	var found string
	deadline := time.Now().Add(3 * time.Second)
	for found == "" && time.Now().Before(deadline) {
		for _, f := range strings.Fields(errw.String()) {
			if strings.HasPrefix(f, "https://idp.example/authorize") {
				found = f
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	if found == "" {
		t.Fatalf("no URL on stderr: %s", errw.String())
	}
	u, _ := url.Parse(found)
	q := u.Query()
	resp, err := http.Get(q.Get("redirect_uri") + "?code=c1&state=" + q.Get("state"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d: %s", code, errw.String())
	}
	if got := readTOML(t, dir+"/config.toml")["api_key"]; got != browserKey {
		t.Errorf("saved %v", got)
	}
}

func TestBrowserLogin_TheRedirectCompletesTheLogin(t *testing.T) {
	for _, tt := range []struct {
		flags []string
		want  float64
	}{{nil, 90}, {[]string{"--expires", "30"}, 30}, {[]string{"--expires", "0"}, 0}} {
		dir := cliEnv(t)
		hub := browserHub(t)
		b := &fakeBrowser{query: approve("c1")}
		r := runBrowser(t, b, time.Second, append([]string{"login", "--endpoint-url", hub.URL}, tt.flags...)...)
		if r.code != 0 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		reqs := hub.requests()
		cb := reqs[1]
		var body map[string]any
		json.Unmarshal([]byte(cb.body), &body)
		redirect := b.lastURL(t).Query().Get("redirect_uri")
		if cb.path != "/api/v1/auth/callback" || cb.header.Get("Authorization") != "" || len(body) != 4 ||
			body["provider"] != "github" || body["code"] != "c1" || body["redirect_uri"] != redirect || body["expires"] != tt.want {
			t.Errorf("callback request %s %v", cb.path, body)
		}
		if b.status != 200 || !strings.HasPrefix(b.contentType, "text/html") {
			t.Errorf("browser got %d %s", b.status, b.contentType)
		}
		var want bytes.Buffer
		json.Indent(&want, []byte(userJSON), "", "  ")
		if r.stdout != want.String()+"\n" || strings.Contains(r.stdout, browserKey) {
			t.Errorf("stdout %q", r.stdout)
		}
		if !strings.Contains(r.stderr, "Logged in as alice") {
			t.Errorf("stderr %q", r.stderr)
		}
		got := readTOML(t, dir+"/config.toml")
		if got["endpoint_url"] != hub.URL || got["api_key"] != browserKey || got["user_id"] != "u-1" {
			t.Errorf("file %v", got)
		}
		u, _ := url.Parse(redirect)
		if c, err := net.Dial("tcp", u.Host); err == nil {
			c.Close()
			t.Error("the redirect listener still accepts connections")
		}
	}
}

func TestBrowserLogin_ABadRedirectFailsTheLogin(t *testing.T) {
	for _, q := range []func(string) string{
		func(string) string { return "code=c1&state=wrong" },
		func(string) string { return "code=c1" },
		func(s string) string { return "error=access_denied&state=" + s },
		func(s string) string { return "state=" + s },
	} {
		dir := cliEnv(t)
		hub := browserHub(t)
		b := &fakeBrowser{query: q}
		r := runBrowser(t, b, time.Second, "login", "--endpoint-url", hub.URL)
		if r.code != 2 {
			t.Fatalf("exit %d: %s", r.code, r.stderr)
		}
		if e := r.envelope(t); e["code"] != float64(0) || e["error_type"] != "client_error" {
			t.Errorf("envelope %v", e)
		}
		if b.status != 400 || !strings.HasPrefix(b.contentType, "text/html") {
			t.Errorf("browser got %d %s", b.status, b.contentType)
		}
		for _, req := range hub.requests() {
			if req.path == "/api/v1/auth/callback" {
				t.Error("the hub's callback was called")
			}
		}
		if _, err := os.Stat(dir); err == nil || r.stdout != "" {
			t.Errorf("config dir exists or stdout %q", r.stdout)
		}
	}
}

func TestBrowserLogin_NoRedirectWithinTheWaitFailsTheLogin(t *testing.T) {
	if DefaultLoginWait != 120*time.Second {
		t.Errorf("DefaultLoginWait = %v", DefaultLoginWait)
	}
	dir := cliEnv(t)
	hub := browserHub(t)
	b := &fakeBrowser{}
	start := time.Now()
	r := runBrowser(t, b, 100*time.Millisecond, "login", "--endpoint-url", hub.URL)
	if r.code != 2 || time.Since(start) >= 2*time.Second {
		t.Fatalf("exit %d after %v: %s", r.code, time.Since(start), r.stderr)
	}
	if e := r.envelope(t); e["code"] != float64(0) || e["error_type"] != "client_error" {
		t.Errorf("envelope %v", e)
	}
	u, _ := url.Parse(b.lastURL(t).Query().Get("redirect_uri"))
	if c, err := net.Dial("tcp", u.Host); err == nil {
		c.Close()
		t.Error("the listener still accepts connections")
	}
	for _, req := range hub.requests() {
		if req.path == "/api/v1/auth/callback" {
			t.Error("the hub's callback was called")
		}
	}
	if _, err := os.Stat(dir); err == nil {
		t.Error("a config dir was created")
	}
}

func TestBrowserLogin_AHubErrorIsPrintedAsReceived(t *testing.T) {
	tests := []struct {
		route  string
		status int
		body   string
	}{
		{"GET /api/v1/auth/providers", 429, `{"error":{"code":429,"message":"too many requests","error_type":"rate_limited"}}`},
		{"POST /api/v1/auth/callback", 400, `{"error":{"code":400,"message":"redirect_uri is not allowed","error_type":"invalid_request"}}`},
		{"POST /api/v1/auth/callback", 401, `{"error":{"code":401,"message":"the provider rejected the code","error_type":"unauthenticated"}}`},
		{"POST /api/v1/auth/callback", 403, `{"error":{"code":403,"message":"user is blocked","error_type":"forbidden"}}`},
		{"POST /api/v1/auth/callback", 409, `{"error":{"code":409,"message":"conflict","error_type":"conflict"}}`},
		{"POST /api/v1/auth/callback", 502, `{"error":{"code":502,"message":"the provider could not be read","error_type":"upstream_error"}}`},
	}
	for _, tt := range tests {
		dir := cliEnv(t)
		hub := browserHub(t)
		hub.answer(tt.route, tt.status, tt.body)
		b := &fakeBrowser{query: approve("c1")}
		r := runBrowser(t, b, time.Second, "login", "--endpoint-url", hub.URL)
		lines := strings.Split(strings.TrimSpace(r.stderr), "\n")
		if r.code != 1 || lines[len(lines)-1] != tt.body || r.stdout != "" {
			t.Errorf("%s %d: exit %d stderr %q", tt.route, tt.status, r.code, r.stderr)
		}
		if _, err := os.Stat(dir); err == nil {
			t.Error("a config dir was created")
		}
		if strings.HasPrefix(tt.route, "GET") && b.calls != 0 {
			t.Error("the browser was opened")
		}
	}
}

func TestBrowserLogin_ALoginThatCannotStartFailsBeforeTheBrowser(t *testing.T) {
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedURL := "http://" + closed.Addr().String()
	closed.Close()
	tests := []struct {
		name    string
		arrange func(h *stubHub)
		args    []string
		want    string
		mention string
	}{
		{"unreachable", nil, nil, "client_error", ""},
		{"unreadable list", func(h *stubHub) { h.answer("GET /api/v1/auth/providers", 200, "hello") }, nil, "client_error", ""},
		{"provider not listed", nil, []string{"--provider", "google"}, "client_error", "github"},
		{"no provider", func(h *stubHub) { h.answer("GET /api/v1/auth/providers", 200, `{"items":[],"next_cursor":null}`) }, nil, "client_error", ""},
		{"bad expires", nil, []string{"--expires", "45"}, "usage_error", ""},
		{"non-numeric expires", nil, []string{"--expires", "x"}, "usage_error", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := cliEnv(t)
			hub := browserHub(t)
			if tt.arrange != nil {
				tt.arrange(hub)
			}
			endpoint := hub.URL
			if tt.name == "unreachable" {
				endpoint = closedURL
			}
			b := &fakeBrowser{query: approve("c1")}
			r := runBrowser(t, b, time.Second, append([]string{"login", "--endpoint-url", endpoint}, tt.args...)...)
			if r.code != 2 {
				t.Fatalf("exit %d: %s", r.code, r.stderr)
			}
			e := r.envelope(t)
			if e["code"] != float64(0) || e["error_type"] != tt.want || !strings.Contains(e["message"].(string), tt.mention) {
				t.Errorf("envelope %v", e)
			}
			if b.calls != 0 || r.stdout != "" {
				t.Errorf("browser calls %d stdout %q", b.calls, r.stdout)
			}
			if _, err := os.Stat(dir); err == nil {
				t.Error("a config dir was created")
			}
			if tt.want == "usage_error" && len(hub.requests()) != 0 {
				t.Error("the hub got a request")
			}
		})
	}
}
