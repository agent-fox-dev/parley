package server

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPipeline_NoAuthenticatorRefusesProtectedRoutes(t *testing.T) {
	handler := &countingHandler{status: 200}
	s := New(Options{})
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: handler.handle})
	for _, opts := range [][]reqOpt{nil, {bearer("anything")}} {
		r := do(t, s.Handler(), "GET", "/api/v1/things", opts...)
		if r.status != http.StatusUnauthorized || r.errorType(t) != "unauthenticated" {
			t.Fatalf("status = %d body %s, want 401 unauthenticated", r.status, r.body)
		}
	}
	if handler.calls.Load() != 0 {
		t.Errorf("handler ran %d times, want 0", handler.calls.Load())
	}
}

func TestPipeline_EveryCheckAndTheHandlerSeeTheirOwnCaller(t *testing.T) {
	type seen struct {
		cred, user string
		admin      bool
	}
	var mu sync.Mutex
	var mismatches []string
	expect := func(r *http.Request, where string) {
		c := CallerFrom(r.Context())
		want := seen{cred: "k1", user: "u1"}
		if r.Header.Get("Authorization") == "Bearer root" {
			want = seen{cred: "admin", admin: true}
		}
		if c == nil || (seen{c.CredentialID, c.UserID, c.Admin}) != want {
			mu.Lock()
			mismatches = append(mismatches, fmt.Sprintf("%s saw %+v, want %+v", where, c, want))
			mu.Unlock()
		}
	}
	s := New(Options{})
	s.SetAuthenticator(func(r *http.Request) (*Caller, error) {
		if r.Header.Get("Authorization") == "Bearer root" {
			return &Caller{CredentialID: "admin", Admin: true}, nil
		}
		return &Caller{CredentialID: "k1", UserID: "u1"}, nil
	})
	s.SetBindingGuard(func(r *http.Request) error { expect(r, "guard"); return nil })
	s.SetScopeCheck(func(r *http.Request) error { expect(r, "scope"); return nil })
	s.AddAccessCheck(func(r *http.Request) error { expect(r, "access"); return nil })
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: func(w http.ResponseWriter, r *http.Request) error {
		time.Sleep(time.Millisecond)
		expect(r, "handler")
		WriteJSON(w, 200, map[string]any{})
		return nil
	}})
	h := s.Handler()

	var wg sync.WaitGroup
	for range 50 {
		for _, cred := range []string{"a", "root"} {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if r := do(t, h, "GET", "/api/v1/things", bearer(cred)); r.status != 200 {
					t.Errorf("status = %d", r.status)
				}
			}()
		}
	}
	wg.Wait()
	for _, m := range mismatches {
		t.Error(m)
	}
}

func TestPipeline_EmptyPositionsPass(t *testing.T) {
	handler := &countingHandler{status: 200}
	s := New(Options{})
	s.SetAuthenticator(bearerAuth(nil))
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: handler.handle})
	if r := do(t, s.Handler(), "GET", "/api/v1/things", bearer("a")); r.status != 200 {
		t.Fatalf("status = %d, want the handler's 200", r.status)
	}
	if handler.calls.Load() != 1 {
		t.Errorf("handler ran %d times, want 1", handler.calls.Load())
	}
}

func TestPipeline_ARequestBlockedInItsHandlerDoesNotHoldUpAnother(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	s := New(Options{})
	s.SetAuthenticator(bearerAuth(nil))
	s.Register(Route{Method: "GET", Pattern: "/things", Handler: func(w http.ResponseWriter, r *http.Request) error {
		if r.URL.Query().Get("hold") == "1" {
			close(entered)
			<-release
		}
		WriteJSON(w, 200, map[string]any{})
		return nil
	}})
	h := s.Handler()

	first := make(chan int, 1)
	go func() { first <- do(t, h, "GET", "/api/v1/things?hold=1", bearer("a")).status }()
	select {
	case <-entered:
	case st := <-first:
		t.Fatalf("the held request returned %d without entering its handler", st)
	}
	second := make(chan int, 1)
	go func() { second <- do(t, h, "GET", "/api/v1/things", bearer("a")).status }()
	select {
	case st := <-second:
		if st != 200 {
			t.Errorf("second request = %d, want 200", st)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the second request waited for the first")
	}
	close(release)
	if st := <-first; st != 200 {
		t.Errorf("first request = %d, want 200", st)
	}
}

func TestPipeline_TheFirstFailingCheckDecidesTheAnswer(t *testing.T) {
	order := []string{"authentication", "feature check", "binding guard", "scope", "access", "handler"}
	tests := []struct {
		failing    string
		status     int
		errorType  string
		body       []byte
		exhaust    bool
		wantPrefix int // how many entries of order ran
	}{
		{"authentication", 401, "unauthenticated", nil, false, 1},
		{"rate limit", 429, "rate_limited", nil, true, 1},
		{"body", 413, "payload_too_large", jsonOfSize(2 << 20), false, 1},
		{"feature check", 503, "unavailable", nil, false, 2},
		{"binding guard", 403, "token_bound", nil, false, 3},
		{"scope", 403, "insufficient_scope", nil, false, 4},
		{"access", 404, "not_found", nil, false, 5},
		{"handler", 400, "invalid_request", nil, false, 6},
		{"nothing", 201, "", nil, false, 6},
	}
	for _, tt := range tests {
		t.Run(tt.failing, func(t *testing.T) {
			log := &callLog{}
			fail := func(name string, e *Error) error {
				if tt.failing == name {
					return e
				}
				return nil
			}
			opts := DefaultOptions()
			opts.RequestsPerMinute = 1000
			if tt.exhaust {
				opts.RequestsPerMinute = 1
			}
			s := New(opts)
			s.SetAuthenticator(func(r *http.Request) (*Caller, error) {
				log.add("authentication")
				if err := fail("authentication", Unauthenticated("no")); err != nil {
					return nil, err
				}
				return &Caller{CredentialID: "k1", UserID: "u1"}, nil
			})
			s.SetBindingGuard(recordingCheck(log, "binding guard", fail("binding guard", TokenBound("bound"))))
			s.SetScopeCheck(recordingCheck(log, "scope", fail("scope", InsufficientScope("scope"))))
			s.AddAccessCheck(recordingCheck(log, "access", fail("access", NotFound("nope"))))
			s.Register(Route{
				Method: "POST", Pattern: "/things",
				Feature: recordingCheck(log, "feature check", fail("feature check", Unavailable("off"))),
				Handler: func(w http.ResponseWriter, r *http.Request) error {
					log.add("handler")
					if err := fail("handler", InvalidRequest("bad")); err != nil {
						return err
					}
					WriteJSON(w, 201, map[string]any{})
					return nil
				},
			})
			h := s.Handler()
			body := tt.body
			if body == nil {
				body = []byte("{}")
			}
			if tt.exhaust {
				do(t, h, "POST", "/api/v1/things", bearer("a"), withBody(body))
				log.reset()
			}
			r := do(t, h, "POST", "/api/v1/things", bearer("a"), withBody(body))
			if r.status != tt.status {
				t.Fatalf("status = %d, want %d: %s", r.status, tt.status, r.body)
			}
			if tt.errorType != "" && r.errorType(t) != tt.errorType {
				t.Errorf("error_type = %q, want %q", r.errorType(t), tt.errorType)
			}
			if got, want := log.get(), order[:tt.wantPrefix]; fmt.Sprint(got) != fmt.Sprint(want) {
				t.Errorf("calls = %v, want %v", got, want)
			}
		})
	}
}

func TestPipeline_APublicRouteSkipsTheCallerChecksButKeepsLimits(t *testing.T) {
	log := &callLog{}
	opts := DefaultOptions()
	opts.RequestsPerMinute = 2
	s := New(opts)
	s.SetAuthenticator(func(*http.Request) (*Caller, error) {
		log.add("authentication")
		return &Caller{CredentialID: "x"}, nil
	})
	s.SetBindingGuard(recordingCheck(log, "binding guard", nil))
	s.SetScopeCheck(recordingCheck(log, "scope", nil))
	s.AddAccessCheck(recordingCheck(log, "access", nil))
	handler := &countingHandler{status: 200}
	s.Register(Route{Method: "POST", Pattern: "/open", Public: true, Handler: handler.handle})
	h := s.Handler()

	if r := do(t, h, "POST", "/api/v1/open", jsonBody("{}")); r.status != 200 || handler.calls.Load() != 1 {
		t.Fatalf("public POST = %d, want the handler's 200", r.status)
	}
	if calls := log.get(); len(calls) != 0 {
		t.Errorf("checks ran on a public route: %v", calls)
	}
	if r := do(t, h, "POST", "/api/v1/open", withBody(jsonOfSize(int(DefaultBodyLimit)+1))); r.status != 413 {
		t.Errorf("over-limit body = %d, want 413", r.status)
	}
	if r := do(t, h, "POST", "/api/v1/open", jsonBody("{}")); r.status != 429 {
		t.Errorf("third request from the address = %d, want 429", r.status)
	}
}

func TestPipeline_UnexpectedFailuresAreInternalAndLogged(t *testing.T) {
	tests := []struct {
		name  string
		setup func(*Server)
	}{
		{"handler returns a plain error", func(s *Server) {
			s.Register(Route{Method: "GET", Pattern: "/fail", Handler: func(http.ResponseWriter, *http.Request) error {
				return errors.New("db password hunter2")
			}})
		}},
		{"handler panics", func(s *Server) {
			s.Register(Route{Method: "GET", Pattern: "/fail", Handler: func(http.ResponseWriter, *http.Request) error {
				panic("boom hunter2")
			}})
		}},
		{"scope check returns a plain error", func(s *Server) {
			s.SetScopeCheck(func(*http.Request) error { return errors.New("scope store hunter2") })
			s.Register(Route{Method: "GET", Pattern: "/fail", Handler: (&countingHandler{status: 200}).handle})
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts := DefaultOptions()
			logger, buf := debugLogger()
			opts.Logger = logger
			s := New(opts)
			s.SetAuthenticator(bearerAuth(nil))
			s.Register(Route{Method: "GET", Pattern: "/fine", Public: true, Handler: (&countingHandler{status: 200}).handle})
			tt.setup(s)
			h := s.Handler()

			r := do(t, h, "GET", "/api/v1/fail", bearer("a"))
			if r.status != 500 {
				t.Fatalf("status = %d, want 500", r.status)
			}
			e := r.errorOf(t)
			if e["error_type"] != "internal" || e["message"] != "internal error" {
				t.Errorf("error = %v, want internal with the fixed message", e)
			}
			if strings.Contains(string(r.body), "hunter2") {
				t.Errorf("the cause leaked into the response: %s", r.body)
			}
			id := r.header.Get("X-Request-ID")
			found := false
			for _, l := range buf.lines() {
				if strings.Contains(l, "hunter2") && strings.Contains(l, id) {
					found = true
				}
			}
			if !found {
				t.Errorf("no log line holds the cause and request id %s: %q", id, buf.lines())
			}
			if r := do(t, h, "GET", "/api/v1/fine"); r.status != 200 {
				t.Errorf("the next request = %d, want 200", r.status)
			}
		})
	}
}
