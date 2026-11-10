package server

import (
	"fmt"
	"net/http"
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
