package identity

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
)

type recordedCall struct {
	method string
	path   string
	header http.Header
	form   url.Values
}

type scripted struct {
	status int
	body   string
	delay  time.Duration
}

// fakeProvider answers /token, /user and /user/emails from a script and
// records every request.
type fakeProvider struct {
	*httptest.Server
	mu     sync.Mutex
	calls  []recordedCall
	script map[string]scripted
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	f := &fakeProvider{script: map[string]scripted{}}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		f.mu.Lock()
		f.calls = append(f.calls, recordedCall{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), form: r.PostForm})
		s, ok := f.script[r.URL.Path]
		f.mu.Unlock()
		if !ok {
			http.NotFound(w, r)
			return
		}
		if s.delay > 0 {
			select {
			case <-time.After(s.delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(s.status)
		io.WriteString(w, s.body)
	}))
	t.Cleanup(f.Close)
	return f
}

func (f *fakeProvider) set(path string, status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.script[path] = scripted{status: status, body: body}
}

func (f *fakeProvider) delayed(path string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.script[path]
	s.delay = d
	f.script[path] = s
}

func (f *fakeProvider) requests() []recordedCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedCall(nil), f.calls...)
}

// provider is a configured provider pointing at the fake.
func (f *fakeProvider) provider(name string) config.Provider {
	return config.Provider{Name: name, ClientID: "gh-id", ClientSecret: "gh-secret",
		AuthorizeURL: f.URL + "/authorize", TokenURL: f.URL + "/token", UserinfoURL: f.URL + "/user"}
}

func (f *fakeProvider) client(timeout time.Duration, names ...string) *OAuthClient {
	var cfg []config.Provider
	for _, n := range names {
		cfg = append(cfg, f.provider(n))
	}
	return NewOAuthClient(ProvidersFrom(cfg), http.DefaultClient, timeout)
}

const redirect = "http://127.0.0.1:5000/callback"

func TestOAuth_GitHubExchangesTheCodeAndReadsTheUser(t *testing.T) {
	f := newFakeProvider(t)
	f.set("/token", 200, `{"access_token":"at-1"}`)
	f.set("/user", 200, `{"id":123,"login":"alice","name":"Alice A","email":"a@x"}`)
	id, err := f.client(time.Second, "github").Identity(context.Background(), "github", "code-1", redirect)
	if err != nil {
		t.Fatal(err)
	}
	if id != (Identity{Provider: "github", ProviderID: "123", Username: "alice", FullName: "Alice A", Email: "a@x"}) {
		t.Errorf("identity = %+v", id)
	}
	calls := f.requests()
	if len(calls) != 2 {
		t.Fatalf("%d calls", len(calls))
	}
	tok := calls[0]
	want := url.Values{"client_id": {"gh-id"}, "client_secret": {"gh-secret"}, "code": {"code-1"}, "redirect_uri": {redirect}, "grant_type": {"authorization_code"}}
	if tok.method != "POST" || tok.header.Get("Accept") != "application/json" || tok.form.Encode() != want.Encode() {
		t.Errorf("token call %s %v %v", tok.method, tok.header.Get("Accept"), tok.form)
	}
	if calls[1].header.Get("Authorization") != "Bearer at-1" {
		t.Errorf("user call Authorization = %q", calls[1].header.Get("Authorization"))
	}
}

func TestOAuth_GitHubReadsThePrimaryVerifiedEmailWhenTheProfileHidesIt(t *testing.T) {
	f := newFakeProvider(t)
	f.set("/token", 200, `{"access_token":"at-1"}`)
	f.set("/user", 200, `{"id":123,"login":"alice","email":null}`)
	f.set("/user/emails", 200, `[{"email":"old@x","primary":false,"verified":true},{"email":"a@x","primary":true,"verified":true}]`)
	id, err := f.client(time.Second, "github").Identity(context.Background(), "github", "code-1", redirect)
	if err != nil || id.Email != "a@x" {
		t.Fatalf("identity %+v, %v", id, err)
	}
	calls := f.requests()
	if len(calls) != 3 || !strings.HasSuffix(calls[2].path, "/emails") || calls[2].header.Get("Authorization") != "Bearer at-1" {
		t.Errorf("calls = %+v", calls)
	}
}

func TestOAuth_GoogleReadsTheVerifiedIdentity(t *testing.T) {
	f := newFakeProvider(t)
	f.set("/token", 200, `{"access_token":"at-1"}`)
	f.set("/user", 200, `{"sub":"1099","name":"Alice Smith","email":"a@x","email_verified":true}`)
	id, err := f.client(time.Second, "google").Identity(context.Background(), "google", "code-1", redirect)
	if err != nil {
		t.Fatal(err)
	}
	if id != (Identity{Provider: "google", ProviderID: "1099", Username: "Alice Smith", FullName: "Alice Smith", Email: "a@x"}) {
		t.Errorf("identity = %+v", id)
	}
}

func TestOAuth_AMissingNameFallsBackToProviderAndID(t *testing.T) {
	tests := []struct {
		provider, user, want string
	}{
		{"github", `{"id":123,"login":"","email":"a@x"}`, "github-123"},
		{"github", `{"id":123,"email":"a@x"}`, "github-123"},
		{"google", `{"sub":"1099","email":"a@x","email_verified":true}`, "google-1099"},
		{"google", `{"sub":"1099","name":"   ","email":"a@x","email_verified":true}`, "google-1099"},
		{"github", `{"id":123,"login":"  alice ","email":"a@x"}`, "alice"},
	}
	for _, tt := range tests {
		f := newFakeProvider(t)
		f.set("/token", 200, `{"access_token":"at-1"}`)
		f.set("/user", 200, tt.user)
		id, err := f.client(time.Second, tt.provider).Identity(context.Background(), tt.provider, "c", redirect)
		if err != nil || id.Username != tt.want {
			t.Errorf("%s %s: %+v, %v; want %s", tt.provider, tt.user, id, err, tt.want)
		}
	}
}

func TestOAuth_AnIdentityWithoutIDOrVerifiedEmailIsAnUpstreamFailure(t *testing.T) {
	tests := []struct {
		provider, user, emails string
	}{
		{"github", `{"login":"alice","email":"a@x"}`, ""},
		{"github", `{"id":123,"login":"alice","email":null}`, `[{"email":"a@x","primary":true,"verified":false},{"email":"b@x","primary":false,"verified":true}]`},
		{"github", `{"id":123,"login":"alice","email":null}`, `[]`},
		{"google", `{"name":"A","email":"a@x","email_verified":true}`, ""},
		{"google", `{"sub":"1","name":"A","email":"a@x","email_verified":false}`, ""},
		{"google", `{"sub":"1","name":"A","email_verified":true}`, ""},
	}
	for _, tt := range tests {
		f := newFakeProvider(t)
		f.set("/token", 200, `{"access_token":"at-1"}`)
		f.set("/user", 200, tt.user)
		if tt.emails != "" {
			f.set("/user/emails", 200, tt.emails)
		}
		id, err := f.client(time.Second, tt.provider).Identity(context.Background(), tt.provider, "c", redirect)
		if KindOf(err) != KindUpstream || id != (Identity{}) {
			t.Errorf("%s %s: %+v, %v; want an upstream failure", tt.provider, tt.user, id, err)
		}
	}
}

func TestOAuth_ARejectedCodeStopsBeforeTheUserIsRead(t *testing.T) {
	for _, tok := range []struct {
		status int
		body   string
	}{{200, `{"error":"bad_verification_code"}`}, {400, `{"error":"invalid_grant"}`}} {
		f := newFakeProvider(t)
		f.set("/token", tok.status, tok.body)
		f.set("/user", 200, `{"id":1,"login":"a","email":"a@x"}`)
		_, err := f.client(time.Second, "github").Identity(context.Background(), "github", "c", redirect)
		if KindOf(err) != KindRejected {
			t.Errorf("%d %s: %v, want rejected", tok.status, tok.body, err)
		}
		if n := len(f.requests()); n != 1 {
			t.Errorf("%d requests, want 1", n)
		}
	}
}

func TestOAuth_AFailingProviderIsAnUpstreamFailureThatLeaksNothing(t *testing.T) {
	closed, _ := net.Listen("tcp", "127.0.0.1:0")
	closedURL := "http://" + closed.Addr().String()
	closed.Close()
	tests := []struct {
		name    string
		arrange func(f *fakeProvider)
		token   string // replaces the token URL
	}{
		{"token URL refused", nil, closedURL + "/token"},
		{"token 503", func(f *fakeProvider) { f.set("/token", 503, `provider-body-text`) }, ""},
		{"token not JSON", func(f *fakeProvider) { f.set("/token", 200, `provider-body-text`) }, ""},
		{"token without access token", func(f *fakeProvider) { f.set("/token", 200, `{}`) }, ""},
		{"user 401", func(f *fakeProvider) {
			f.set("/token", 200, `{"access_token":"at-1"}`)
			f.set("/user", 401, `provider-body-text`)
		}, ""},
		{"user 500", func(f *fakeProvider) {
			f.set("/token", 200, `{"access_token":"at-1"}`)
			f.set("/user", 500, `provider-body-text`)
		}, ""},
		{"user not an object", func(f *fakeProvider) { f.set("/token", 200, `{"access_token":"at-1"}`); f.set("/user", 200, `[1,2]`) }, ""},
		{"e-mail list 500", func(f *fakeProvider) {
			f.set("/token", 200, `{"access_token":"at-1"}`)
			f.set("/user", 200, `{"id":1,"login":"a","email":null}`)
			f.set("/user/emails", 500, `provider-body-text`)
		}, ""},
	}
	for _, tt := range tests {
		f := newFakeProvider(t)
		if tt.arrange != nil {
			tt.arrange(f)
		}
		p := f.provider("github")
		if tt.token != "" {
			p.TokenURL = tt.token
		}
		client := NewOAuthClient(ProvidersFrom([]config.Provider{p}), http.DefaultClient, time.Second)
		_, err := client.Identity(context.Background(), "github", "code-1", redirect)
		if KindOf(err) != KindUpstream {
			t.Errorf("%s: %v, want upstream", tt.name, err)
			continue
		}
		for _, secret := range []string{"code-1", "gh-secret", "at-1", "provider-body-text"} {
			if strings.Contains(err.Error(), secret) {
				t.Errorf("%s: error %q carries %s", tt.name, err, secret)
			}
		}
	}
}

func TestOAuth_ASlowProviderTimesOut(t *testing.T) {
	for _, slow := range []string{"/token", "/user"} {
		f := newFakeProvider(t)
		f.set("/token", 200, `{"access_token":"at-1"}`)
		f.set("/user", 200, `{"id":1,"login":"a","email":"a@x"}`)
		f.delayed(slow, 500*time.Millisecond)
		start := time.Now()
		_, err := f.client(50*time.Millisecond, "github").Identity(context.Background(), "github", "c", redirect)
		if KindOf(err) != KindUpstream {
			t.Errorf("slow %s: %v, want upstream", slow, err)
		}
		if d := time.Since(start); d >= 300*time.Millisecond {
			t.Errorf("slow %s: returned after %v", slow, d)
		}
	}
	if DefaultProviderTimeout != 15*time.Second {
		t.Errorf("default timeout = %v", DefaultProviderTimeout)
	}
}
