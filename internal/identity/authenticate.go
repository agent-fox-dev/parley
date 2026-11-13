package identity

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/config"
	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// MarkNeedsUser is the key of server.Route.Marks that declares a route as
// needing a user: the authenticator answers the admin token, which has none,
// 403 forbidden there. Its value is not read.
const MarkNeedsUser = "identity.needs_user"

// The credential types a Principal reports.
const (
	TypeAdmin  = "admin"
	TypeAPIKey = "api_key"
	TypePAT    = "pat"
)

// adminCredentialID is the credential id of the admin token.
const adminCredentialID = "admin"

// Principal is the caller of a request as authentication found it. It
// marshals to JSON as the REST API shows a caller.
type Principal struct {
	// Type is TypeAdmin, TypeAPIKey or TypePAT.
	Type string `json:"type"`
	// CredentialID is "admin", the key_id or the token_id.
	CredentialID string `json:"credential_id"`
	// User is the user the credential acts for; nil for the admin token.
	User *User `json:"user"`
	// Permissions, Paths and Workspace are a PAT's; nil for the other
	// types. Paths is nil for a PAT without path limits, and Workspace for
	// one that is not bound.
	Permissions []string `json:"permissions"`
	Paths       []string `json:"paths"`
	Workspace   *string  `json:"workspace"`
}

// PrincipalOf returns the caller the identity service authenticated for the
// request whose context ctx is. It reports false on a public route.
func PrincipalOf(ctx context.Context) (Principal, bool) {
	c, ok := server.CallerOf(ctx)
	if !ok {
		return Principal{}, false
	}
	p, ok := c.Detail.(*Principal)
	if !ok || p == nil {
		return Principal{}, false
	}
	return *p, true
}

// Deps are what a Service works with.
type Deps struct {
	// Store holds the identity records and the admin settings.
	Store *store.Store
	// Now is the clock; nil means time.Now.
	Now func() time.Time
	// Providers are the configured identity providers, in file order.
	Providers []config.Provider
	// ExternalURL is [server].external_url, "" when unset.
	ExternalURL string
}

// Service is the identity part of the hub: it authenticates the requests
// to non-public routes. The zero Service is ready to be registered; Bind
// gives it its dependencies before it serves the first request.
type Service struct {
	deps Deps
}

// Bind sets the service's dependencies. The hub calls it once, from a start
// hook, before it serves any request.
func (s *Service) Bind(d Deps) {
	if d.Now == nil {
		d.Now = time.Now
	}
	s.deps = d
}

// errAuthRequired answers a request without a credential of a known
// format.
func errAuthRequired() error { return server.Unauthenticated("authentication required") }

// errInvalid answers a credential that is not valid, without saying which
// part of it was wrong.
func errInvalid() error { return server.Unauthenticated("invalid credentials") }

// Authenticate is the pipeline's authenticator. It reads the credential
// from the Authorization header, scheme Bearer in any letter case followed
// by one or more spaces, white space around the credential ignored, checks
// it with VerifyCredential, and reports the caller with its Principal as
// the Caller's Detail. A credential anywhere else is not read. On a route
// marked MarkNeedsUser it answers the admin token 403 forbidden.
func (s *Service) Authenticate(r *http.Request) (server.Caller, error) {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(r.Header.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return server.Caller{}, errAuthRequired()
	}
	p, err := s.VerifyCredential(r.Context(), strings.TrimSpace(rest))
	if err != nil {
		return server.Caller{}, err
	}
	if p.Type == TypeAdmin {
		if m, ok := server.MatchedRoute(r); ok {
			if _, needsUser := m.Route.Marks[MarkNeedsUser]; needsUser {
				return server.Caller{}, server.Forbidden("this route needs a user")
			}
		}
	}
	c := server.Caller{CredentialID: p.CredentialID, Admin: p.Type == TypeAdmin, Detail: &p}
	if p.User != nil {
		c.UserID = p.User.ID
	}
	return c, nil
}

// VerifyCredential checks a credential given as a string, the same check
// the Authorization header gets, for callers that read it from elsewhere.
// It returns the credential's Principal, or the *server.Error that answers
// it: 401 "authentication required" for a string of no known format, 401
// "invalid credentials" for an unknown id, a wrong secret or an admin-format
// token that is not the admin token. Any other error is the store's.
func (s *Service) VerifyCredential(ctx context.Context, credential string) (Principal, error) {
	c, ok := parseCredential(credential)
	if !ok {
		return Principal{}, errAuthRequired()
	}
	switch c.typ {
	case TypeAdmin:
		ok, err := VerifyAdminToken(ctx, s.deps.Store, c.secret)
		if err != nil {
			return Principal{}, err
		}
		if !ok {
			return Principal{}, errInvalid()
		}
		return Principal{Type: TypeAdmin, CredentialID: adminCredentialID}, nil
	case TypeAPIKey:
		k, err := APIKeyByID(ctx, s.deps.Store.Main(), c.id)
		if err := checkSecret(c.secret, k.hash, err); err != nil {
			return Principal{}, err
		}
		return s.withUser(ctx, Principal{Type: TypeAPIKey, CredentialID: k.KeyID}, k.UserID)
	default:
		t, err := PATByID(ctx, s.deps.Store.Main(), c.id)
		if err := checkSecret(c.secret, t.hash, err); err != nil {
			return Principal{}, err
		}
		p := Principal{Type: TypePAT, CredentialID: t.TokenID, Permissions: t.Permissions, Paths: t.Paths}
		if t.Workspace != "" {
			p.Workspace = &t.Workspace
		}
		return s.withUser(ctx, p, t.UserID)
	}
}

// checkSecret compares the hash of secret with stored, the hash of the
// record a lookup found or failed to find with lookupErr. An unknown id is
// compared with dummyHash, so that it takes the same path as a wrong
// secret.
func checkSecret(secret, stored string, lookupErr error) error {
	found := lookupErr == nil
	if !found {
		if !errors.Is(lookupErr, ErrNotFound) {
			return lookupErr
		}
		stored = dummyHash
	}
	match := subtle.ConstantTimeCompare([]byte(hashSecret(secret)), []byte(stored)) == 1
	if !found || !match {
		return errInvalid()
	}
	return nil
}

// withUser fills in the user the credential of p acts for.
func (s *Service) withUser(ctx context.Context, p Principal, userID string) (Principal, error) {
	u, err := UserByID(ctx, s.deps.Store.Main(), userID)
	if err != nil {
		return Principal{}, err
	}
	p.User = &u
	return p, nil
}
