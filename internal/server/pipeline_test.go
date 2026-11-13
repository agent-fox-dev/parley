package server

import (
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestPipeline_RefusesANonPublicRouteWithoutAnAuthenticator(t *testing.T) {
	t.Parallel()
	var log callLog
	s := New(Options{})
	s.Register(Route{Method: http.MethodGet, Pattern: "/things", Handler: log.handler("handler", http.StatusOK)})
	h := s.Handler()

	for _, cv := range credentialVariants {
		t.Run(cv.name, func(t *testing.T) {
			rec := do(h, newRequest(http.MethodGet, "/api/v1/things", cv.header, ""))

			assertError(t, rec, http.StatusUnauthorized, "unauthenticated")
			if calls := log.calls(); len(calls) != 0 {
				t.Errorf("ran %v, want no handler", calls)
			}
		})
	}
}

func TestPipeline_GivesEachCheckAndTheHandlerTheCallerOfItsOwnRequest(t *testing.T) {
	t.Parallel()
	callers := map[string]Caller{
		"Bearer a":    {CredentialID: "k1", UserID: "u1"},
		"Bearer root": {CredentialID: "admin", Admin: true},
	}
	var observed atomic.Int64
	observe := func(position string, r *http.Request) {
		observed.Add(1)
		want := callers[r.Header.Get("Authorization")]
		if got, ok := CallerOf(r.Context()); !ok || got != want {
			t.Errorf("%s of a request with %q saw caller %+v (%t), want %+v", position, r.Header.Get("Authorization"), got, ok, want)
		}
	}
	observer := func(position string) Check {
		return func(r *http.Request) error {
			observe(position, r)
			return nil
		}
	}

	s := New(Options{})
	s.SetAuthenticator(func(r *http.Request) (Caller, error) {
		c, ok := callers[r.Header.Get("Authorization")]
		if !ok {
			return Caller{}, Unauthenticated("unknown credential")
		}
		return c, nil
	})
	s.SetBindingGuard(observer("binding guard"))
	s.SetScopeCheck(observer("scope check"))
	s.AddAccessCheck(observer("access check"))
	s.Register(Route{
		Method:  http.MethodGet,
		Pattern: "/things",
		Feature: observer("feature check"),
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			observe("handler", r)
			WriteJSON(w, http.StatusOK, map[string]string{})
			return nil
		},
	})
	h := s.Handler()

	const rounds = 50
	var wg sync.WaitGroup
	for range rounds {
		for credential := range callers {
			wg.Go(func() {
				rec := do(h, newRequest(http.MethodGet, "/api/v1/things", http.Header{"Authorization": {credential}}, ""))
				if rec.Code != http.StatusOK {
					t.Errorf("request with %q: status = %d, want %d (body %s)", credential, rec.Code, http.StatusOK, rec.Body.Bytes())
				}
			})
		}
	}
	wg.Wait()

	// Five positions read the caller: feature check, binding guard, scope
	// check, access check and handler.
	if got, want := observed.Load(), int64(rounds*len(callers)*5); got != want {
		t.Errorf("positions observed %d times, want %d", got, want)
	}
}

func TestPipeline_TreatsAnEmptyPositionAsPassed(t *testing.T) {
	t.Parallel()
	var log callLog
	s := New(Options{})
	s.SetAuthenticator(log.authenticator(testCaller, nil))
	s.Register(Route{Method: http.MethodGet, Pattern: "/things", Handler: log.handler("handler", http.StatusOK)})

	rec := do(s.Handler(), newRequest(http.MethodGet, "/api/v1/things", nil, ""))

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want %d (body %s)", rec.Code, http.StatusOK, rec.Body.Bytes())
	}
	if got, want := log.calls(), []string{"authentication", "handler"}; !slices.Equal(got, want) {
		t.Errorf("ran %v, want %v", got, want)
	}
}

func TestPipeline_RunsAccessChecksInInstallationOrderWithTheMatchedRoute(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name       string
		adminErr   error
		wantStatus int
		wantCalls  []string
	}{
		{
			name:       "every access check passes",
			wantStatus: http.StatusCreated,
			wantCalls:  []string{"authentication", "binding guard", "scope check", "admin check", "workspace check", "handler"},
		},
		{
			name:       "the first access check fails",
			adminErr:   Forbidden("admins only"),
			wantStatus: http.StatusForbidden,
			wantCalls:  []string{"authentication", "binding guard", "scope check", "admin check"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			var log callLog
			// seesRoute wraps a check so that it also asserts the matched
			// route it is given.
			seesRoute := func(c Check) Check {
				return func(r *http.Request) error {
					m, ok := MatchedRoute(r)
					if !ok || m.Method != http.MethodPost || m.Pattern != "/workspaces/:slug/tokens" ||
						m.Params["slug"] != "ws1" || m.Route.Scope != "tokens:write" || m.Route.Marks["access"] != "owner" {
						t.Errorf("check saw matched route %+v (%t), want POST /workspaces/:slug/tokens with slug ws1", m, ok)
					}
					return c(r)
				}
			}
			s := New(Options{})
			s.SetAuthenticator(log.authenticator(testCaller, nil))
			s.SetBindingGuard(seesRoute(log.check("binding guard", nil)))
			s.SetScopeCheck(seesRoute(log.check("scope check", nil)))
			s.AddAccessCheck(seesRoute(log.check("admin check", tt.adminErr)))
			s.AddAccessCheck(seesRoute(log.check("workspace check", nil)))
			s.Register(Route{
				Method:  http.MethodPost,
				Pattern: "/workspaces/:slug/tokens",
				Scope:   "tokens:write",
				Marks:   map[string]string{"access": "owner"},
				Handler: log.handler("handler", http.StatusCreated),
			})

			rec := do(s.Handler(), newRequest(http.MethodPost, "/api/v1/workspaces/ws1/tokens", nil, ""))

			if rec.Code != tt.wantStatus {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, tt.wantStatus, rec.Body.Bytes())
			}
			if got := log.calls(); !slices.Equal(got, tt.wantCalls) {
				t.Errorf("ran %v, want %v", got, tt.wantCalls)
			}
		})
	}
}

func TestPipeline_ServesARequestWhileAnotherIsBlockedInItsHandler(t *testing.T) {
	t.Parallel()
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	releaseFirst := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseFirst)

	s := New(Options{})
	s.SetAuthenticator(func(*http.Request) (Caller, error) { return testCaller, nil })
	s.Register(Route{
		Method:  http.MethodGet,
		Pattern: "/things",
		Handler: func(w http.ResponseWriter, r *http.Request) error {
			if r.URL.Query().Get("hold") == "1" {
				close(entered)
				<-release
			}
			WriteJSON(w, http.StatusOK, map[string]string{})
			return nil
		},
	})
	h := s.Handler()

	first := make(chan int, 1)
	go func() { first <- do(h, newRequest(http.MethodGet, "/api/v1/things?hold=1", nil, "")).Code }()
	waitFor(t, entered, 5*time.Second, "held handler entered")

	second := make(chan int, 1)
	go func() { second <- do(h, newRequest(http.MethodGet, "/api/v1/things", nil, "")).Code }()
	if code := waitFor(t, second, 2*time.Second, "second request while the first is held"); code != http.StatusOK {
		t.Errorf("second request: status = %d, want %d", code, http.StatusOK)
	}
	select {
	case code := <-first:
		t.Fatalf("held request finished with %d before it was released", code)
	default:
	}

	releaseFirst()
	if code := waitFor(t, first, 2*time.Second, "held request after release"); code != http.StatusOK {
		t.Errorf("held request: status = %d, want %d", code, http.StatusOK)
	}
}
