package server

import (
	"context"
	"errors"
	"net/http"
)

// Caller is who a request comes from, as the authenticator reports it.
type Caller struct {
	// CredentialID identifies the credential used; the rate limit counts by
	// it.
	CredentialID string
	// UserID is the user the credential belongs to; empty for the admin
	// token.
	UserID string
	// Admin is true for the admin token.
	Admin bool
	// Principal is the authenticator's own description of the caller (the
	// credential's type, the user, a token's permissions); the package that
	// installed the authenticator reads it back.
	Principal any
}

// Authenticator authenticates a request to a route that is not public. It
// returns the caller, or an error that is the request's answer (normally
// 401 unauthenticated).
type Authenticator func(r *http.Request) (*Caller, error)

// CallerFrom returns the caller the authenticator reported for the request,
// or nil on a public route.
func CallerFrom(ctx context.Context) *Caller {
	c, _ := ctx.Value(keyCaller).(*Caller)
	return c
}

// SetAuthenticator installs the authenticator. Without one, every route
// that is not public answers 401 unauthenticated.
func (s *Server) SetAuthenticator(a Authenticator) { s.auth = a }

// SetBindingGuard installs the binding guard.
func (s *Server) SetBindingGuard(c Check) { s.guard = c }

// SetScopeCheck installs the scope check.
func (s *Server) SetScopeCheck(c Check) { s.scope = c }

// AddAccessCheck installs an access check; the access position runs every
// one installed, in installation order.
func (s *Server) AddAccessCheck(c Check) { s.access = append(s.access, c) }

// runPipeline runs the checks of a matched REST request in their fixed order
// — authentication, rate limit, body, the route's feature check, binding
// guard, scope, access — and then its handler; the first failure is the
// answer.
func (s *Server) runPipeline(w http.ResponseWriter, r *http.Request, m *Match) {
	tw := &trackingWriter{ResponseWriter: w}
	if err := s.pipeline(tw, r, m.Route); err != nil && !tw.wrote {
		s.writeFailure(tw, r, err)
	}
}

func (s *Server) pipeline(w http.ResponseWriter, r *http.Request, rt *Route) error {
	var caller *Caller
	if !rt.Public {
		var err error
		caller, err = s.authenticate(r)
		if err != nil {
			// A refused request still counts against its client address,
			// but the authenticator's answer is the request's answer.
			s.limiter.Allow(rateKey(r, nil))
			return err
		}
		r = r.WithContext(context.WithValue(r.Context(), keyCaller, caller))
	}
	if err := s.checkRate(w, r, caller); err != nil {
		return err
	}
	r, err := s.checkBody(r, rt)
	if err != nil {
		return err
	}
	// A public route skips steps 5 to 7: the feature check, the binding
	// guard, the scope check and the access checks.
	var checks []Check
	if !rt.Public {
		checks = append(checks, rt.Feature, s.guard, s.scope)
		checks = append(checks, s.access...)
	}
	for _, check := range checks {
		if check == nil {
			continue
		}
		if err := check(r); err != nil {
			return err
		}
	}
	if rt.Create && r.Method == http.MethodPost {
		return s.runCreate(w, r, rt)
	}
	return rt.Handler(w, r)
}

// authenticate runs the installed authenticator; without one, every request
// is refused.
func (s *Server) authenticate(r *http.Request) (*Caller, error) {
	if s.auth == nil {
		return nil, Unauthenticated("authentication is required")
	}
	caller, err := s.auth(r)
	if err != nil {
		return nil, err
	}
	if caller == nil {
		return nil, Unauthenticated("authentication is required")
	}
	return caller, nil
}

// writeFailure answers err: an *Error as its envelope, anything else as
// 500 internal.
func (s *Server) writeFailure(w http.ResponseWriter, r *http.Request, err error) {
	var apiErr *Error
	if errors.As(err, &apiErr) {
		WriteError(w, apiErr)
		return
	}
	s.logger.Error("internal error", "request_id", RequestID(r.Context()), "error", err.Error())
	WriteError(w, Internal("internal error"))
}

// trackingWriter records whether and with which status a response was
// started.
type trackingWriter struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (t *trackingWriter) WriteHeader(status int) {
	if !t.wrote {
		t.status, t.wrote = status, true
	}
	t.ResponseWriter.WriteHeader(status)
}

func (t *trackingWriter) Write(b []byte) (int, error) {
	if !t.wrote {
		t.status, t.wrote = http.StatusOK, true
	}
	return t.ResponseWriter.Write(b)
}

// statusOrOK is the status written, or 200 when nothing was.
func (t *trackingWriter) statusOrOK() int {
	if !t.wrote {
		return http.StatusOK
	}
	return t.status
}

// Unwrap lets http.ResponseController reach the underlying writer.
func (t *trackingWriter) Unwrap() http.ResponseWriter { return t.ResponseWriter }
