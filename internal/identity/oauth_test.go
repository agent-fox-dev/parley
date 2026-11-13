package identity_test

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/identity"
)

// The paths the fake provider answers: its token URL, its user URL and
// GitHub's e-mail list under the user URL.
const (
	fakeTokenPath  = "/token"
	fakeUserPath   = "/user"
	fakeEmailsPath = "/user/emails"
)

// fakeProvider is an identity provider served by httptest. It answers each
// path from a script the test sets, the token URL with {"access_token":
// "at-1"} until told otherwise and an unscripted path with 404, and records
// every request it receives.
type fakeProvider struct {
	srv *httptest.Server

	mu       sync.Mutex
	answers  map[string]fakeAnswer
	requests []fakeRequest

	// abandoned counts the delayed answers whose client went away first.
	abandoned atomic.Int64
}

type fakeAnswer struct {
	status int // 0 is 200
	body   string
	delay  time.Duration
}

type fakeRequest struct {
	method, path string
	header       http.Header
	form         url.Values // the form-encoded body; empty for a GET
}

func newFakeProvider(t *testing.T) *fakeProvider {
	t.Helper()
	f := &fakeProvider{answers: map[string]fakeAnswer{}}
	f.answer(fakeTokenPath, http.StatusOK, map[string]any{"access_token": "at-1"})
	f.srv = httptest.NewServer(http.HandlerFunc(f.serve))
	t.Cleanup(f.srv.Close)
	return f
}

// answer scripts path's answer: body as it is when it is a string, else
// marshalled to JSON.
func (f *fakeProvider) answer(path string, status int, body any) {
	raw, ok := body.(string)
	if !ok {
		b, err := json.Marshal(body)
		if err != nil {
			panic(err)
		}
		raw = string(b)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.answers[path] = fakeAnswer{status: status, body: raw, delay: f.answers[path].delay}
}

// delay holds path's answer back by d, or until the client goes away.
func (f *fakeProvider) delay(path string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a := f.answers[path]
	a.delay = d
	f.answers[path] = a
}

func (f *fakeProvider) serve(w http.ResponseWriter, r *http.Request) {
	r.ParseForm()
	f.mu.Lock()
	f.requests = append(f.requests, fakeRequest{method: r.Method, path: r.URL.Path, header: r.Header.Clone(), form: r.PostForm})
	a, ok := f.answers[r.URL.Path]
	f.mu.Unlock()
	if !ok {
		http.NotFound(w, r)
		return
	}
	select {
	case <-time.After(a.delay):
	case <-r.Context().Done():
		f.abandoned.Add(1)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(cmp.Or(a.status, http.StatusOK))
	io.WriteString(w, a.body)
}

// provider is the configuration of the provider name with its endpoints on
// the fake. Its client id and secret are gh-id and gh-secret for github,
// gg-id and gg-secret for google.
func (f *fakeProvider) provider(name string) config.Provider {
	short := map[string]string{"github": "gh", "google": "gg"}[name]
	return config.Provider{
		Name: name, ClientID: short + "-id", ClientSecret: short + "-secret",
		AuthorizeURL: f.srv.URL + "/authorize", TokenURL: f.srv.URL + fakeTokenPath, UserinfoURL: f.srv.URL + fakeUserPath,
	}
}

// recorded returns the requests received so far, in order.
func (f *fakeProvider) recorded() []fakeRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.requests)
}

func (f *fakeProvider) paths() []string {
	var paths []string
	for _, r := range f.recorded() {
		paths = append(paths, r.path)
	}
	return paths
}

// errorKind names which of the client's error kinds err is.
func errorKind(err error) string {
	switch rejected, upstream := errors.Is(err, identity.ErrRejected), errors.Is(err, identity.ErrUpstream); {
	case rejected && !upstream:
		return "rejected"
	case upstream && !rejected:
		return "upstream"
	}
	return fmt.Sprintf("neither or both kinds: %v", err)
}

const redirectURI = "http://127.0.0.1:5000/callback"

func TestOAuthClient_ReadsTheIdentityTheProviderVouchesFor(t *testing.T) {
	t.Parallel()
	type object = map[string]any
	emails := []object{{"email": "old@x", "primary": false, "verified": true}, {"email": "a@x", "primary": true, "verified": true}}
	github := func(username, fullName string) identity.Identity {
		return identity.Identity{Provider: "github", ProviderID: "123", Username: username, FullName: fullName, Email: "a@x"}
	}
	google := func(username, fullName string) identity.Identity {
		return identity.Identity{Provider: "google", ProviderID: "1099", Username: username, FullName: fullName, Email: "a@x"}
	}
	cases := []struct {
		name, provider string
		user           object
		readsEmails    bool
		want           identity.Identity
	}{
		{"github", "github", object{"id": 123, "login": "alice", "name": "Alice A", "email": "a@x"}, false, github("alice", "Alice A")},
		{"github hiding the address", "github", object{"id": 123, "login": "alice", "email": nil}, true, github("alice", "")},
		{"github with an empty address", "github", object{"id": 123, "login": "alice", "email": ""}, true, github("alice", "")},
		{"github with a padded login", "github", object{"id": 123, "login": "  alice ", "email": "a@x"}, false, github("alice", "")},
		{"github with an empty login", "github", object{"id": 123, "login": "", "email": "a@x"}, false, github("github-123", "")},
		{"github without a login", "github", object{"id": 123, "email": "a@x"}, false, github("github-123", "")},
		{"google", "google", object{"sub": "1099", "name": "Alice Smith", "email": "a@x", "email_verified": true}, false, google("Alice Smith", "Alice Smith")},
		{"google without a name", "google", object{"sub": "1099", "email": "a@x", "email_verified": true}, false, google("google-1099", "")},
		{"google with a blank name", "google", object{"sub": "1099", "name": "   ", "email": "a@x", "email_verified": true}, false, google("google-1099", "   ")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeProvider(t)
			f.answer(fakeUserPath, http.StatusOK, tc.user)
			f.answer(fakeEmailsPath, http.StatusOK, emails)
			p := f.provider(tc.provider)
			client := identity.NewOAuthClient([]config.Provider{p}, nil, 0)

			got, err := client.Identity(context.Background(), tc.provider, "code-1", redirectURI)
			if err != nil {
				t.Fatalf("Identity: %v", err)
			}
			if got != tc.want {
				t.Errorf("identity = %+v, want %+v", got, tc.want)
			}
			wantPaths := []string{fakeTokenPath, fakeUserPath}
			if tc.readsEmails {
				wantPaths = append(wantPaths, fakeEmailsPath)
			}
			if paths := f.paths(); !slices.Equal(paths, wantPaths) {
				t.Fatalf("requested %v, want %v", paths, wantPaths)
			}
			reqs := f.recorded()
			wantForm := url.Values{"client_id": {p.ClientID}, "client_secret": {p.ClientSecret}, "code": {"code-1"},
				"redirect_uri": {redirectURI}, "grant_type": {"authorization_code"}}
			if r := reqs[0]; r.method != http.MethodPost || r.header.Get("Accept") != "application/json" || !maps.EqualFunc(r.form, wantForm, slices.Equal) {
				t.Errorf("token request = %s Accept %q form %v, want POST Accept application/json form %v", r.method, r.header.Get("Accept"), r.form, wantForm)
			}
			for _, r := range reqs[1:] {
				if r.method != http.MethodGet || r.header.Get("Authorization") != "Bearer at-1" {
					t.Errorf("%s = %s Authorization %q, want GET Bearer at-1", r.path, r.method, r.header.Get("Authorization"))
				}
			}
		})
	}
}

func TestOAuthClient_ReportsARejectedCodeOrAFailingProvider(t *testing.T) {
	t.Parallel()
	type object = map[string]any
	type reply struct {
		status int
		body   any
	}
	githubUser := reply{http.StatusOK, object{"id": 123, "login": "alice", "email": "a@x"}}
	hiddenAddress := reply{http.StatusOK, object{"id": 123, "login": "alice", "email": nil}}
	token, user, emails := fakeTokenPath, fakeUserPath, fakeEmailsPath
	cases := []struct {
		name, provider string
		refused        bool // the token URL refuses the connection
		replies        map[string]reply
		want           string
		wantPaths      []string
	}{
		{"token 200 with an error member", "github", false, map[string]reply{token: {http.StatusOK, object{"error": "bad_verification_code"}}},
			"rejected", []string{token}},
		{"token 400 with an error member", "github", false, map[string]reply{token: {http.StatusBadRequest, object{"error": "invalid_grant"}}},
			"rejected", []string{token}},
		{"token URL refusing the connection", "github", true, nil, "upstream", nil},
		{"token 503", "github", false, map[string]reply{token: {http.StatusServiceUnavailable, "provider body: down"}}, "upstream", []string{token}},
		{"token 503 with an error member", "github", false, map[string]reply{token: {http.StatusServiceUnavailable, object{"error": "temporarily_unavailable"}}},
			"upstream", []string{token}},
		{"token 200 not JSON", "github", false, map[string]reply{token: {http.StatusOK, "not json"}}, "upstream", []string{token}},
		{"token 200 without an access token", "github", false, map[string]reply{token: {http.StatusOK, object{}}}, "upstream", []string{token}},
		{"user URL 401", "github", false, map[string]reply{user: {http.StatusUnauthorized, object{"message": "Bad credentials"}}},
			"upstream", []string{token, user}},
		{"user URL 500", "github", false, map[string]reply{user: {http.StatusInternalServerError, "provider body: broken"}},
			"upstream", []string{token, user}},
		{"user URL 200 not an object", "github", false, map[string]reply{user: {http.StatusOK, "[1,2]"}}, "upstream", []string{token, user}},
		{"github user without an id", "github", false, map[string]reply{user: {http.StatusOK, object{"login": "alice", "email": "a@x"}}},
			"upstream", []string{token, user}},
		{"github without a primary verified address", "github", false, map[string]reply{user: hiddenAddress,
			emails: {http.StatusOK, []object{{"email": "a@x", "primary": true, "verified": false}, {"email": "b@x", "primary": false, "verified": true}}}},
			"upstream", []string{token, user, emails}},
		{"github with an empty e-mail list", "github", false, map[string]reply{user: hiddenAddress, emails: {http.StatusOK, []object{}}},
			"upstream", []string{token, user, emails}},
		{"e-mail list URL 500", "github", false, map[string]reply{user: hiddenAddress, emails: {http.StatusInternalServerError, "provider body: broken"}},
			"upstream", []string{token, user, emails}},
		{"google without a sub", "google", false, map[string]reply{user: {http.StatusOK, object{"name": "Alice", "email": "a@x", "email_verified": true}}},
			"upstream", []string{token, user}},
		{"google with an unverified address", "google", false, map[string]reply{user: {http.StatusOK, object{"sub": "1099", "email": "a@x", "email_verified": false}}},
			"upstream", []string{token, user}},
		{"google verified without an address", "google", false, map[string]reply{user: {http.StatusOK, object{"sub": "1099", "email_verified": true}}},
			"upstream", []string{token, user}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			f := newFakeProvider(t)
			f.answer(user, githubUser.status, githubUser.body)
			for path, r := range tc.replies {
				f.answer(path, r.status, r.body)
			}
			p := f.provider(tc.provider)
			if tc.refused {
				closed := httptest.NewServer(http.NotFoundHandler())
				closed.Close()
				p.TokenURL = closed.URL + fakeTokenPath
			}
			client := identity.NewOAuthClient([]config.Provider{p}, nil, 0)

			got, err := client.Identity(context.Background(), tc.provider, "code-1", redirectURI)
			if kind := errorKind(err); kind != tc.want {
				t.Errorf("error kind = %s, want %s", kind, tc.want)
			}
			if got != (identity.Identity{}) {
				t.Errorf("identity = %+v, want none", got)
			}
			if paths := f.paths(); !slices.Equal(paths, tc.wantPaths) {
				t.Errorf("requested %v, want %v", paths, tc.wantPaths)
			}
			secrets := []string{"code-1", p.ClientSecret, "at-1"}
			for path := range tc.replies {
				secrets = append(secrets, f.answers[path].body)
			}
			for _, s := range secrets {
				if err != nil && strings.Contains(err.Error(), s) {
					t.Errorf("error %q holds %q", err, s)
				}
			}
		})
	}
}

func TestOAuthClient_AbandonsACallThatOutlastsTheTimeout(t *testing.T) {
	t.Parallel()
	for _, slow := range []string{fakeTokenPath, fakeUserPath} {
		t.Run(strings.TrimPrefix(slow, "/")+" URL", func(t *testing.T) {
			t.Parallel()
			f := newFakeProvider(t)
			f.answer(fakeUserPath, http.StatusOK, map[string]any{"id": 123, "login": "alice", "email": "a@x"})
			f.delay(slow, 500*time.Millisecond)
			client := identity.NewOAuthClient([]config.Provider{f.provider("github")}, nil, 50*time.Millisecond)

			start := time.Now()
			got, err := client.Identity(context.Background(), "github", "code-1", redirectURI)
			elapsed := time.Since(start)
			if kind := errorKind(err); kind != "upstream" {
				t.Errorf("error kind = %s, want upstream", kind)
			}
			if got != (identity.Identity{}) {
				t.Errorf("identity = %+v, want none", got)
			}
			if elapsed >= 300*time.Millisecond {
				t.Errorf("Identity returned after %v, want under 300ms", elapsed)
			}
			// The provider sees the connection closed before its answer.
			for deadline := time.Now().Add(time.Second); f.abandoned.Load() == 0; time.Sleep(5 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatal("the delayed call was not abandoned")
				}
			}
		})
	}
}
