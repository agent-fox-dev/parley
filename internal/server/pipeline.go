package server

import (
	"context"
	"errors"
	"net/http"
	"slices"
)

// Check is one check of the pipeline: a route's feature check, the binding
// guard, the scope check or an access check. It returns nil to let the
// request go on, or the error that answers it, usually an *Error.
type Check func(r *http.Request) error

// Authenticator identifies the caller of a request from its credential. It
// returns the caller, or the error that answers the request when the
// credential is missing or not valid.
type Authenticator func(r *http.Request) (Caller, error)

// Caller is who sent a request, as the authenticator reports it: the id of
// the credential, and either the user it acts for or the admin token.
type Caller struct {
	// CredentialID identifies the credential the request carried.
	CredentialID string
	// UserID is the user the credential acts for; empty for the admin token.
	UserID string
	// Admin marks the admin token.
	Admin bool
}

type callerKey struct{}

// CallerOf returns the caller the authenticator reported for the request
// whose context ctx is. It reports false on a public route and before
// authentication.
func CallerOf(ctx context.Context) (Caller, bool) {
	c, ok := ctx.Value(callerKey{}).(Caller)
	return c, ok
}

// positions are the checks later packages install. The pipeline copies them
// once per request, so no lock is held while a check or a handler runs.
type positions struct {
	authenticator Authenticator
	bindingGuard  Check
	scopeCheck    Check
	accessChecks  []Check
}

// SetAuthenticator installs the authenticator. Until one is installed, every
// route that is not public answers 401 unauthenticated.
func (s *Server) SetAuthenticator(a Authenticator) {
	s.posMu.Lock()
	defer s.posMu.Unlock()
	s.pos.authenticator = a
}

// SetBindingGuard installs the binding guard, which runs after the route's
// feature check.
func (s *Server) SetBindingGuard(c Check) {
	s.posMu.Lock()
	defer s.posMu.Unlock()
	s.pos.bindingGuard = c
}

// SetScopeCheck installs the scope check, which runs after the binding
// guard.
func (s *Server) SetScopeCheck(c Check) {
	s.posMu.Lock()
	defer s.posMu.Unlock()
	s.pos.scopeCheck = c
}

// AddAccessCheck adds an access check after those already installed. The
// access checks run after the scope check, in the order they were added;
// each decides from the matched route whether it applies.
func (s *Server) AddAccessCheck(c Check) {
	s.posMu.Lock()
	defer s.posMu.Unlock()
	s.pos.accessChecks = append(slices.Clone(s.pos.accessChecks), c)
}

func (s *Server) installed() positions {
	s.posMu.RLock()
	defer s.posMu.RUnlock()
	return s.pos
}

// runPipeline runs the checks of a request to rt in their fixed order, then
// the handler. The first error ends the request and answers it.
func (s *Server) runPipeline(w http.ResponseWriter, r *http.Request, rt Route) {
	if err := s.pipeline(w, r, rt); err != nil {
		writeErr(w, err)
	}
}

func (s *Server) pipeline(w http.ResponseWriter, r *http.Request, rt Route) error {
	pos := s.installed()

	if !rt.Public {
		if pos.authenticator == nil {
			return Unauthenticated("authentication required")
		}
		caller, err := pos.authenticator(r)
		if err != nil {
			return err
		}
		r = r.WithContext(context.WithValue(r.Context(), callerKey{}, caller))
	}

	if err := s.rateLimit(r); err != nil {
		return err
	}
	r, err := s.readBody(r)
	if err != nil {
		return err
	}

	if !rt.Public {
		checks := []Check{rt.Feature, pos.bindingGuard, pos.scopeCheck}
		for _, c := range append(checks, pos.accessChecks...) {
			if c == nil {
				continue
			}
			if err := c(r); err != nil {
				return err
			}
		}
	}

	return rt.Handler(w, r)
}

// rateLimit is the rate-limit stage; it passes every request for now.
func (s *Server) rateLimit(*http.Request) error { return nil }

// readBody is the body stage; it passes every request unchanged for now.
func (s *Server) readBody(r *http.Request) (*http.Request, error) { return r, nil }

// writeErr answers err: an *Error as its envelope, anything else as an
// internal error whose text stays out of the response.
func writeErr(w http.ResponseWriter, err error) {
	var e *Error
	if !errors.As(err, &e) {
		e = Internal("internal error")
	}
	WriteError(w, e)
}
