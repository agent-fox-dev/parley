package identity

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// MarkNeedsUser is the route mark (package 02's named route declarations)
// of a route that needs a user; the authenticator answers the admin token
// 403 there.
const MarkNeedsUser = "needs_user"

// Principal is an authenticated caller as the routes behind authentication
// read it.
type Principal struct {
	// Type is admin, api_key or pat.
	Type string `json:"type"`
	// CredentialID is admin, a key_id or a token_id.
	CredentialID string `json:"credential_id"`
	// User is the credential's user; nil for the admin token.
	User *User `json:"user"`
	// Permissions, Paths and Workspace are set for a PAT only.
	Permissions []string `json:"permissions"`
	Paths       []string `json:"paths"`
	Workspace   *string  `json:"workspace"`
}

// PrincipalFrom returns the authenticated caller of a request, or nil.
func PrincipalFrom(ctx context.Context) *Principal {
	c := server.CallerFrom(ctx)
	if c == nil {
		return nil
	}
	p, _ := c.Principal.(*Principal)
	return p
}

// The fixed answers of authentication.
var (
	errAuthRequired = func() error { return server.Unauthenticated("authentication required") }
	errInvalid      = func() error { return server.Unauthenticated("invalid credentials") }
	errNeedsUser    = func() error { return server.Forbidden("this route needs a user") }
)

// Authenticator checks credentials against the store.
type Authenticator struct {
	st  *store.Store
	now func() time.Time
}

// NewAuthenticator builds the authenticator of the request pipeline over the
// store, with a clock (time.Now in production).
func NewAuthenticator(st *store.Store, now func() time.Time) server.Authenticator {
	a := &Authenticator{st: st, now: now}
	return a.authenticate
}

// NewChecker returns the same credential check for callers that hold a
// credential string, such as the git server with a Basic password.
func NewChecker(st *store.Store, now func() time.Time) *Authenticator {
	return &Authenticator{st: st, now: now}
}

// bearerCredential reads the credential of an Authorization header: the
// scheme Bearer in any letter case, one or more spaces, the credential with
// surrounding white space ignored.
func bearerCredential(header string) string {
	scheme, rest, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	return strings.TrimSpace(rest)
}

func (a *Authenticator) authenticate(r *http.Request) (*server.Caller, error) {
	p, err := a.Check(r.Context(), bearerCredential(r.Header.Get("Authorization")))
	if err != nil {
		return nil, err
	}
	if p.User == nil {
		if m := server.MatchFrom(r.Context()); m != nil && m.Route.Marks[MarkNeedsUser] != "" {
			return nil, errNeedsUser()
		}
	}
	return toCaller(p), nil
}

func toCaller(p *Principal) *server.Caller {
	c := &server.Caller{CredentialID: p.CredentialID, Principal: p}
	if p.User == nil {
		c.Admin = true
	} else {
		c.UserID = p.User.ID
	}
	return c
}

// Check authenticates a credential string. It answers, in this order:
// unknown format "authentication required"; unknown id or wrong secret
// "invalid credentials"; then, only once the secret matched, "credential
// revoked", "credential expired" and 403 "user is blocked".
func (a *Authenticator) Check(ctx context.Context, credential string) (*Principal, error) {
	cred, ok := parseCredential(credential)
	if !ok {
		return nil, errAuthRequired()
	}
	if cred.kind == TypeAdmin {
		ok, err := VerifyAdminToken(ctx, a.st, cred.whole)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, errInvalid()
		}
		return &Principal{Type: TypeAdmin, CredentialID: AdminCredentialID}, nil
	}
	stored, err := lookupCredential(ctx, a.st.DB(), cred.kind, cred.id)
	if errors.Is(err, ErrNotFound) {
		secretMatches(cred.secret, dummyHash)
		return nil, errInvalid()
	}
	if err != nil {
		return nil, err
	}
	if !secretMatches(cred.secret, stored.secretHash) {
		return nil, errInvalid()
	}
	user, err := GetUser(ctx, a.st.DB(), stored.userID)
	if errors.Is(err, ErrNotFound) {
		return nil, errInvalid()
	}
	if err != nil {
		return nil, err
	}
	p := &Principal{Type: cred.kind, CredentialID: cred.id, User: &user}
	if cred.kind == TypePAT {
		p.Permissions, p.Paths, p.Workspace = stored.permissions, stored.paths, stored.workspace
		if p.Permissions == nil {
			p.Permissions = []string{}
		}
	}
	return p, nil
}
