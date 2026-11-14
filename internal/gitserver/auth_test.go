package gitserver_test

import (
	"encoding/base64"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/agent-fox-dev/parley/internal/hubtest"
	"github.com/agent-fox-dev/parley/internal/identity"
)

func TestAuth_TakesTheCredentialFromTheBasicPasswordAlone(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	passwords := []struct {
		name, password, user string
		admin                bool
	}{
		{"api key", w.ownerKey, w.owner, false},
		{"pat", w.ownerPAT, w.owner, false},
		{"admin token", w.Token, "", true},
	}
	for _, p := range passwords {
		for _, name := range []string{"", "x-token-auth", "git", "member"} {
			t.Run(p.name+"/"+name, func(t *testing.T) {
				if a := w.call(t, http.MethodGet, demoRefs, basic(name, p.password)); a.status != http.StatusOK {
					t.Fatalf("status = %d %q, want 200", a.status, a.body)
				}
				if c := w.service.last(t).Caller; c.UserID != p.user || c.Admin != p.admin {
					t.Errorf("caller = user %q admin %v, want user %q admin %v", c.UserID, c.Admin, p.user, p.admin)
				}
			})
		}
	}
}

func TestAuth_ChallengesEveryMissingOrFailedCredentialAlike(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{})
	revoked := w.IssueKey(t, w.owner)
	if status, body := w.Call(t, http.MethodDelete, "/api/v1/users/"+w.owner+"/keys/"+revoked[len("parley_"):len("parley_")+8], w.Token, nil); status != http.StatusOK {
		t.Fatalf("revoke the key = %d %s, want 200", status, body)
	}
	expired := w.IssuePAT(t, identity.PATSpec{UserID: w.owner, Permissions: []string{"git:read"}, ExpiresAt: new(w.clock().Add(-1)),
		CreatedAt: w.clock().Add(-2)})
	headers := []struct{ name, auth string }{
		{"none", ""},
		{"bearer", "Bearer " + w.ownerKey},
		{"not base64", "Basic !!!"},
		{"no colon", "Basic " + base64.StdEncoding.EncodeToString([]byte("nocolon"))},
		{"empty password", basic("x", "")},
		{"unknown key", basic("x", "parley_abcdefgh_"+strings.Repeat("x", 32))},
		{"revoked key", basic("x", revoked)},
		{"expired pat", basic("x", expired)},
	}
	var first string
	for _, h := range headers {
		for _, path := range []struct{ method, path string }{{http.MethodGet, demoRefs}, {http.MethodPost, nosuchPush}} {
			t.Run(h.name+"/"+path.method, func(t *testing.T) {
				a := w.call(t, path.method, path.path, h.auth)
				if a.status != http.StatusUnauthorized || a.header.Get("WWW-Authenticate") != `Basic realm="parley"` || a.body != "authentication required\n" {
					t.Fatalf("answer = %d WWW-Authenticate %q %q, want 401 Basic realm=\"parley\" \"authentication required\\n\"",
						a.status, a.header.Get("WWW-Authenticate"), a.body)
				}
				if first == "" {
					first = a.dump()
				} else if got := a.dump(); got != first {
					t.Errorf("answer =\n%s\nwant the same as the first:\n%s", got, first)
				}
			})
		}
	}
	if n := w.service.served(); n != 0 {
		t.Errorf("%d refused requests reached the service, want none", n)
	}
}

// The two requests under the limit are one REST request and one git
// request, so that the test also shows that git counts against the window
// the REST API counts the credential in.
func TestAuth_RateLimitsTheCredentialBeforeAnyLaterCheck(t *testing.T) {
	t.Parallel()
	w := newWorld(t, hubtest.Config{TOML: "[limits]\nrequests_per_minute = 2"})
	if status, body := w.Call(t, http.MethodGet, "/api/v1/user", w.ownerKey, nil); status == http.StatusTooManyRequests {
		t.Fatalf("first request = %d %s, want it admitted", status, body)
	}
	if a := w.call(t, http.MethodGet, demoRefs, basic("x", w.ownerKey)); a.status == http.StatusTooManyRequests {
		t.Fatalf("second request = %d %q, want it admitted", a.status, a.body)
	}
	served := w.service.served()
	for _, path := range []string{demoRefs, "/git/acme/nosuch.git/info/refs?service=git-upload-pack"} {
		t.Run(path, func(t *testing.T) {
			a := w.call(t, http.MethodGet, path, basic("x", w.ownerKey))
			retry, err := strconv.Atoi(a.header.Get("Retry-After"))
			if a.status != http.StatusTooManyRequests || err != nil || retry < 1 || a.body != "rate limit exceeded\n" ||
				a.header.Get("Content-Type") != "text/plain; charset=utf-8" {
				t.Errorf("answer = %d Retry-After %q %q, want 429 with whole seconds of at least 1 \"rate limit exceeded\\n\"",
					a.status, a.header.Get("Retry-After"), a.body)
			}
		})
	}
	if n := w.service.served(); n != served {
		t.Errorf("%d refused requests reached the service, want none", n-served)
	}
	for range 3 {
		if a := w.call(t, http.MethodGet, demoRefs, basic("x", "wrong")); a.status != http.StatusUnauthorized {
			t.Errorf("a wrong password over the address's limit = %d %q, want 401", a.status, a.body)
		}
	}
}
