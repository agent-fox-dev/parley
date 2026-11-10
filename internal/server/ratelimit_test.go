package server

import (
	"net/http"
	"testing"
	"time"
)

// limitedServer has GET /things behind bearerAuth, a public GET /open and a
// handler counter, on a test clock.
type limitedServer struct {
	*Server
	clock   *fakeClock
	handler *countingHandler
}

func newLimitedServer(limit int) *limitedServer {
	clock := newFakeClock()
	opts := DefaultOptions()
	opts.RequestsPerMinute = limit
	opts.Now = clock.Now
	l := &limitedServer{Server: New(opts), clock: clock, handler: &countingHandler{status: 200}}
	l.SetAuthenticator(bearerAuth(nil))
	l.Register(Route{Method: "GET", Pattern: "/things", Handler: l.handler.handle})
	l.Register(Route{Method: "GET", Pattern: "/open", Public: true, Handler: l.handler.handle})
	return l
}

func TestRateLimit_RefusesACredentialOverItsLimit(t *testing.T) {
	s := newLimitedServer(3)
	h := s.Handler()
	for i := range 3 {
		if r := do(t, h, "GET", "/api/v1/things", bearer("a")); r.status != 200 {
			t.Fatalf("request %d = %d, want 200", i+1, r.status)
		}
	}
	s.clock.advance(20400 * time.Millisecond)
	r := do(t, h, "GET", "/api/v1/things", bearer("a"))
	if r.status != http.StatusTooManyRequests || r.errorType(t) != "rate_limited" {
		t.Fatalf("fourth request = %d %s, want 429 rate_limited", r.status, r.body)
	}
	if ra := r.header.Get("Retry-After"); ra != "40" {
		t.Errorf("Retry-After = %q, want 40", ra)
	}
	if n := s.handler.calls.Load(); n != 3 {
		t.Errorf("handler ran %d times, want 3", n)
	}

	def := New(DefaultOptions())
	def.SetAuthenticator(bearerAuth(nil))
	def.Register(Route{Method: "GET", Pattern: "/things", Handler: (&countingHandler{status: 200}).handle})
	dh := def.Handler()
	for i := 1; i <= 600; i++ {
		if r := do(t, dh, "GET", "/api/v1/things", bearer("a")); r.status != 200 {
			t.Fatalf("default options: request %d = %d, want 200", i, r.status)
		}
	}
	if r := do(t, dh, "GET", "/api/v1/things", bearer("a")); r.status != 429 {
		t.Fatalf("default options: request 601 = %d, want 429", r.status)
	}
}

func TestRateLimit_CountsCredentialsSeparately(t *testing.T) {
	s := newLimitedServer(3)
	h := s.Handler()
	for i := range 4 {
		r := do(t, h, "GET", "/api/v1/things", bearer("a"))
		if want := map[bool]int{true: 429, false: 200}[i == 3]; r.status != want {
			t.Fatalf("a #%d = %d, want %d", i+1, r.status, want)
		}
	}
	for i := range 4 {
		r := do(t, h, "GET", "/api/v1/things", bearer("b"))
		if want := map[bool]int{true: 429, false: 200}[i == 3]; r.status != want {
			t.Fatalf("b #%d = %d, want %d", i+1, r.status, want)
		}
	}
}

func TestRateLimit_RequestsWithoutAValidCredentialCountByAddress(t *testing.T) {
	s := newLimitedServer(3)
	h := s.Handler()
	for i := range 3 {
		if r := do(t, h, "GET", "/api/v1/things", bearer("bad"), fromAddr("10.0.0.1")); r.status != 401 {
			t.Fatalf("bad credential #%d = %d, want 401", i+1, r.status)
		}
	}
	r := do(t, h, "GET", "/api/v1/open", fromAddr("10.0.0.1"))
	if r.status != 429 || r.errorType(t) != "rate_limited" || r.header.Get("Retry-After") == "" {
		t.Fatalf("public route from an over-limit address = %d %s, want 429 with Retry-After", r.status, r.body)
	}
	if r := do(t, h, "GET", "/api/v1/things", bearer("bad"), fromAddr("10.0.0.1")); r.status != 401 {
		t.Errorf("bad credential over the limit = %d, want the authenticator's 401", r.status)
	}
	if r := do(t, h, "GET", "/api/v1/open", fromAddr("10.0.0.2")); r.status != 200 {
		t.Errorf("another address = %d, want 200", r.status)
	}
	if r := do(t, h, "GET", "/api/v1/open", fromAddr("10.0.0.1"), header("X-Forwarded-For", "10.9.9.9")); r.status != 429 {
		t.Errorf("forwarding header changed the address: %d, want 429", r.status)
	}
}

func TestRateLimit_ZeroTurnsTheLimitOff(t *testing.T) {
	s := newLimitedServer(0)
	h := s.Handler()
	for range 2000 {
		if r := do(t, h, "GET", "/api/v1/things", bearer("a")); r.status == 429 {
			t.Fatal("a request was refused with the limit off")
		}
		if r := do(t, h, "GET", "/api/v1/open"); r.status == 429 {
			t.Fatal("a public request was refused with the limit off")
		}
	}
}

func TestRateLimit_ProbesAndUnmatchedRequestsAreNeitherCountedNorRefused(t *testing.T) {
	s := newLimitedServer(3)
	h := s.Handler()
	requests := []struct {
		method, path string
		want         int
	}{
		{"GET", "/healthz", 200}, {"GET", "/readyz", 200}, {"GET", "/version", 200},
		{"GET", "/metrics", 404}, {"GET", "/api/v1/nope", 404}, {"DELETE", "/api/v1/open", 405},
	}
	for _, rq := range requests {
		for range 50 {
			if r := do(t, h, rq.method, rq.path); r.status != rq.want {
				t.Fatalf("%s %s = %d, want %d", rq.method, rq.path, r.status, rq.want)
			}
		}
	}
	for i := range 3 {
		if r := do(t, h, "GET", "/api/v1/open"); r.status != 200 {
			t.Fatalf("GET /open #%d = %d, want 200 (the probes were counted)", i+1, r.status)
		}
	}
	if r := do(t, h, "GET", "/api/v1/open"); r.status != 429 {
		t.Fatalf("fourth GET /open = %d, want 429", r.status)
	}
	if r := do(t, h, "GET", "/healthz"); r.status != 200 {
		t.Errorf("/healthz from an over-limit address = %d, want 200", r.status)
	}
}

func TestRateLimit_ANewWindowStartsAfterSixtySeconds(t *testing.T) {
	s := newLimitedServer(3)
	h := s.Handler()
	for range 3 {
		do(t, h, "GET", "/api/v1/things", bearer("a"))
	}
	s.clock.advance(59 * time.Second)
	r := do(t, h, "GET", "/api/v1/things", bearer("a"))
	if r.status != 429 || r.header.Get("Retry-After") != "1" {
		t.Fatalf("at 59 s = %d with Retry-After %q, want 429 and 1", r.status, r.header.Get("Retry-After"))
	}
	s.clock.advance(time.Second)
	for i := range 3 {
		if r := do(t, h, "GET", "/api/v1/things", bearer("a")); r.status != 200 {
			t.Fatalf("new window #%d = %d, want 200", i+1, r.status)
		}
	}
	if r := do(t, h, "GET", "/api/v1/things", bearer("a")); r.status != 429 {
		t.Fatalf("fourth in the new window = %d, want 429", r.status)
	}
	again := newLimitedServer(3)
	if r := do(t, again.Handler(), "GET", "/api/v1/things", bearer("a")); r.status != 200 {
		t.Errorf("a new server's first request = %d, want 200", r.status)
	}
}

func TestRateLimit_RefusedRequestsCountAndDoNotExtendTheWindow(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*limitedServer)
		opts  []reqOpt
		want  int
	}{
		{"scope check refuses", func(s *limitedServer) {
			s.SetScopeCheck(func(*http.Request) error { return InsufficientScope("no scope") })
		}, nil, 403},
		{"handler refuses", func(s *limitedServer) {
			s.Register(Route{Method: "POST", Pattern: "/things", Handler: func(http.ResponseWriter, *http.Request) error {
				return InvalidRequest("bad")
			}})
		}, []reqOpt{jsonBody("{}")}, 400},
		{"body check refuses", func(s *limitedServer) {
			s.Register(Route{Method: "POST", Pattern: "/things", Handler: s.handler.handle})
		}, []reqOpt{withBody(jsonOfSize(int(DefaultBodyLimit) + 1))}, 413},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := newLimitedServer(3)
			tt.setup(s)
			h := s.Handler()
			method := "GET"
			if tt.opts != nil {
				method = "POST"
			}
			send := func() apiResponse {
				return do(t, h, method, "/api/v1/things", append([]reqOpt{bearer("a")}, tt.opts...)...)
			}
			for i := range 3 {
				if r := send(); r.status != tt.want {
					t.Fatalf("#%d = %d, want %d", i+1, r.status, tt.want)
				}
			}
			if r := send(); r.status != 429 {
				t.Fatalf("fourth = %d, want 429", r.status)
			}
			s.clock.advance(30 * time.Second)
			for range 10 {
				if r := send(); r.status != 429 {
					t.Fatalf("at 30 s = %d, want 429", r.status)
				}
			}
			s.clock.advance(30 * time.Second)
			if r := send(); r.status == 429 {
				t.Fatal("at 60 s the request was still refused: the 429s extended the window")
			}
		})
	}
}
