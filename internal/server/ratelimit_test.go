package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// limitServer builds a server over opts with an authenticator that takes
// "Bearer <id>" as credential <id> and refuses "Bearer bad" or a request
// without a bearer token with 401, a protected route GET /things and a
// public route GET /open. Both handlers record their route's name in log.
func limitServer(opts Options, log *callLog) http.Handler {
	s := New(opts)
	s.SetAuthenticator(func(r *http.Request) (Caller, error) {
		id, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || id == "bad" {
			return Caller{}, Unauthenticated("credential not valid")
		}
		return Caller{CredentialID: id, UserID: "user-" + id}, nil
	})
	s.Register(Route{Method: http.MethodGet, Pattern: "/things", Handler: log.handler("things", http.StatusOK)})
	s.Register(Route{Method: http.MethodGet, Pattern: "/open", Public: true, Handler: log.handler("open", http.StatusOK)})
	return s.Handler()
}

// limitRequest builds a request with "Authorization: Bearer <credential>"
// unless credential is empty, from the client address addr unless addr is
// empty.
func limitRequest(method, path, credential, addr string) *http.Request {
	var header http.Header
	if credential != "" {
		header = http.Header{"Authorization": {"Bearer " + credential}}
	}
	r := newRequest(method, path, header, "")
	if addr != "" {
		r.RemoteAddr = addr
	}
	return r
}

// assertRetryAfter fails the test unless rec carries Retry-After retryAfter.
func assertRetryAfter(t *testing.T, rec *httptest.ResponseRecorder, retryAfter string) {
	t.Helper()
	if got := rec.Header().Get("Retry-After"); got != retryAfter {
		t.Errorf("Retry-After = %q, want %q", got, retryAfter)
	}
}

// count returns how often name occurs in calls.
func count(calls []string, name string) int {
	n := 0
	for _, c := range calls {
		if c == name {
			n++
		}
	}
	return n
}

func TestRateLimit_RefusesACredentialOverTheLimitBeforeTheHandler(t *testing.T) {
	t.Parallel()
	t.Run("a limit of 3", func(t *testing.T) {
		t.Parallel()
		var log callLog
		clock := newTestClock()
		h := limitServer(Options{RequestsPerMinute: 3, Now: clock.Now}, &log)

		for i := range 3 {
			if rec := do(h, limitRequest(http.MethodGet, "/api/v1/things", "a", "")); rec.Code != http.StatusOK {
				t.Fatalf("request %d: status = %d, want %d", i+1, rec.Code, http.StatusOK)
			}
		}
		clock.Advance(20400 * time.Millisecond)
		rec := do(h, limitRequest(http.MethodGet, "/api/v1/things", "a", ""))

		assertError(t, rec, http.StatusTooManyRequests, "rate_limited")
		assertRetryAfter(t, rec, "40")
		if got := count(log.calls(), "things"); got != 3 {
			t.Errorf("handler ran %d times, want 3", got)
		}
	})
	t.Run("the default of 600", func(t *testing.T) {
		t.Parallel()
		var log callLog
		h := limitServer(Options{}, &log)

		for i := range 600 {
			if rec := do(h, limitRequest(http.MethodGet, "/api/v1/things", "a", "")); rec.Code != http.StatusOK {
				t.Fatalf("request %d: status = %d, want %d", i+1, rec.Code, http.StatusOK)
			}
		}
		rec := do(h, limitRequest(http.MethodGet, "/api/v1/things", "a", ""))
		assertError(t, rec, http.StatusTooManyRequests, "rate_limited")
	})
}

func TestRateLimit_CountsEachCredentialSeparately(t *testing.T) {
	t.Parallel()
	var log callLog
	h := limitServer(Options{RequestsPerMinute: 3, Now: newTestClock().Now}, &log)
	const addr = "10.0.0.1:5000"

	for i, want := range []int{200, 200, 200, 429} {
		if rec := do(h, limitRequest(http.MethodGet, "/api/v1/things", "a", addr)); rec.Code != want {
			t.Errorf("credential a, request %d: status = %d, want %d", i+1, rec.Code, want)
		}
	}
	for i, want := range []int{200, 200, 200, 429} {
		if rec := do(h, limitRequest(http.MethodGet, "/api/v1/things", "b", addr)); rec.Code != want {
			t.Errorf("credential b from the same address, request %d: status = %d, want %d", i+1, rec.Code, want)
		}
	}
}

func TestRateLimit_CountsARequestWithoutAValidCredentialAgainstItsAddress(t *testing.T) {
	t.Parallel()
	var log callLog
	h := limitServer(Options{RequestsPerMinute: 3, Now: newTestClock().Now}, &log)
	const first, second = "10.0.0.1:5000", "10.0.0.2:5000"

	for i := range 3 {
		rec := do(h, limitRequest(http.MethodGet, "/api/v1/things", "bad", first))
		if rec.Code != http.StatusUnauthorized {
			t.Fatalf("refused credential, request %d: status = %d, want %d", i+1, rec.Code, http.StatusUnauthorized)
		}
	}

	rec := do(h, limitRequest(http.MethodGet, "/api/v1/open", "", first))
	assertError(t, rec, http.StatusTooManyRequests, "rate_limited")
	assertRetryAfter(t, rec, "60")

	rec = do(h, limitRequest(http.MethodGet, "/api/v1/things", "bad", first))
	assertError(t, rec, http.StatusUnauthorized, "unauthenticated")

	if rec := do(h, limitRequest(http.MethodGet, "/api/v1/open", "", second)); rec.Code != http.StatusOK {
		t.Errorf("another address: status = %d, want %d", rec.Code, http.StatusOK)
	}

	forwarded := limitRequest(http.MethodGet, "/api/v1/open", "", first)
	forwarded.Header.Set("X-Forwarded-For", "10.9.9.9")
	assertError(t, do(h, forwarded), http.StatusTooManyRequests, "rate_limited")

	if got := count(log.calls(), "open"); got != 1 {
		t.Errorf("public handler ran %d times, want 1", got)
	}
}

func TestRateLimit_AdmitsEveryRequestWhenTurnedOff(t *testing.T) {
	t.Parallel()
	var log callLog
	h := limitServer(Options{RequestsPerMinute: 3, RateLimitDisabled: true, Now: newTestClock().Now}, &log)

	for _, r := range []struct{ path, credential string }{{"/api/v1/things", "a"}, {"/api/v1/open", ""}} {
		for i := range 2000 {
			if rec := do(h, limitRequest(http.MethodGet, r.path, r.credential, "10.0.0.1:5000")); rec.Code != http.StatusOK {
				t.Fatalf("%s request %d: status = %d, want %d", r.path, i+1, rec.Code, http.StatusOK)
			}
		}
	}
}

func TestRateLimit_NeverCountsBuiltInRoutesOrRequestsRefusedAtRouteMatching(t *testing.T) {
	t.Parallel()
	var log callLog
	h := limitServer(Options{RequestsPerMinute: 3, Now: newTestClock().Now}, &log)
	const addr = "10.0.0.1:5000"

	probes := []struct {
		method, path string
		want         int
	}{
		{http.MethodGet, "/healthz", http.StatusOK},
		{http.MethodGet, "/readyz", http.StatusOK},
		{http.MethodGet, "/version", http.StatusOK},
		{http.MethodGet, "/metrics", http.StatusNotFound},
		{http.MethodGet, "/api/v1/nope", http.StatusNotFound},
		{http.MethodDelete, "/api/v1/open", http.StatusMethodNotAllowed},
	}
	for _, p := range probes {
		for i := range 50 {
			if rec := do(h, limitRequest(p.method, p.path, "", addr)); rec.Code != p.want {
				t.Fatalf("%s %s, request %d: status = %d, want %d", p.method, p.path, i+1, rec.Code, p.want)
			}
		}
	}

	for i, want := range []int{200, 200, 200, 429} {
		if rec := do(h, limitRequest(http.MethodGet, "/api/v1/open", "", addr)); rec.Code != want {
			t.Errorf("public route, request %d: status = %d, want %d", i+1, rec.Code, want)
		}
	}
	for _, p := range probes {
		if rec := do(h, limitRequest(p.method, p.path, "", addr)); rec.Code != p.want {
			t.Errorf("%s %s from an address over the limit: status = %d, want %d", p.method, p.path, rec.Code, p.want)
		}
	}
}

func TestRateLimit_StartsANewWindowSixtySecondsAfterItsFirstRequest(t *testing.T) {
	t.Parallel()
	var log callLog
	clock := newTestClock()
	opts := Options{RequestsPerMinute: 3, Now: clock.Now}
	h := limitServer(opts, &log)
	get := func() *http.Request { return limitRequest(http.MethodGet, "/api/v1/things", "a", "") }

	for range 3 {
		do(h, get())
	}
	clock.Advance(59 * time.Second)
	rec := do(h, get())
	assertError(t, rec, http.StatusTooManyRequests, "rate_limited")
	assertRetryAfter(t, rec, "1")

	clock.Advance(time.Second)
	for i, want := range []int{200, 200, 200, 429} {
		if rec := do(h, get()); rec.Code != want {
			t.Errorf("new window, request %d: status = %d, want %d", i+1, rec.Code, want)
		}
	}

	restarted := limitServer(opts, &log)
	if rec := do(restarted, get()); rec.Code != http.StatusOK {
		t.Errorf("a new server: status = %d, want %d", rec.Code, http.StatusOK)
	}
}

func TestRateLimit_CountsARequestWhateverItsLaterAnswer(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name       string
		scopeErr   error
		handlerErr error
		body       string
		wantStatus int
		wantType   string
	}{
		{name: "refused by the scope check", scopeErr: InsufficientScope("scope missing"), body: "{}", wantStatus: 403, wantType: "insufficient_scope"},
		{name: "refused by the handler", handlerErr: InvalidRequest("bad"), body: "{}", wantStatus: 400, wantType: "invalid_request"},
		{name: "refused by the body check", body: strings.Repeat("x", 17), wantStatus: 413, wantType: "payload_too_large"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			clock := newTestClock()
			s := New(Options{RequestsPerMinute: 3, Now: clock.Now})
			s.SetAuthenticator(func(*http.Request) (Caller, error) { return testCaller, nil })
			s.SetScopeCheck(func(*http.Request) error { return tc.scopeErr })
			s.Register(Route{
				Method:      http.MethodPost,
				Pattern:     "/things",
				MaxBodySize: 16,
				Handler: func(w http.ResponseWriter, _ *http.Request) error {
					if tc.handlerErr != nil {
						return tc.handlerErr
					}
					WriteJSON(w, http.StatusCreated, map[string]string{})
					return nil
				},
			})
			h := s.Handler()
			post := func() *http.Request { return newRequest(http.MethodPost, "/api/v1/things", jsonHeader, tc.body) }

			for range 3 {
				assertError(t, do(h, post()), tc.wantStatus, tc.wantType)
			}
			assertError(t, do(h, post()), http.StatusTooManyRequests, "rate_limited")

			clock.Advance(30 * time.Second)
			for range 10 {
				rec := do(h, post())
				assertError(t, rec, http.StatusTooManyRequests, "rate_limited")
				assertRetryAfter(t, rec, "30")
			}

			clock.Advance(30 * time.Second)
			assertError(t, do(h, post()), tc.wantStatus, tc.wantType)
		})
	}
}

func TestLimiter_AdmitsExactlyTheLimitUnderConcurrentUse(t *testing.T) {
	t.Parallel()
	l := NewLimiter(100, newTestClock().Now)
	var admitted atomic.Int64
	var wg sync.WaitGroup
	for range 500 {
		wg.Go(func() {
			if ok, _ := l.Allow("k"); ok {
				admitted.Add(1)
			}
		})
	}
	wg.Wait()
	if got := admitted.Load(); got != 100 {
		t.Errorf("admitted %d requests, want 100", got)
	}
}

func TestLimiter_DropsAWindowOnceItHasEnded(t *testing.T) {
	t.Parallel()
	clock := newTestClock()
	l := NewLimiter(1, clock.Now)
	for i := range 100 {
		l.Allow(fmt.Sprintf("key-%d", i))
	}

	clock.Advance(time.Minute)
	l.Allow("later")

	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) != 1 {
		t.Errorf("limiter holds %d windows, want 1 (only the open one)", len(l.windows))
	}
}

func TestLimiter_AdmitsEverythingWithALimitOfZero(t *testing.T) {
	t.Parallel()
	l := NewLimiter(0, newTestClock().Now)
	for i := range 2000 {
		if ok, _ := l.Allow("k"); !ok {
			t.Fatalf("request %d refused, want every request admitted", i+1)
		}
	}
}
