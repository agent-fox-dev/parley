package gitserver_test

import (
	"net/http"
	"strconv"
	"testing"

	"github.com/agent-fox-dev/parley/internal/hubtest"
)

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
