package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// The limits of a token request: the name is counted in characters, and
// the lifetime is one of a few fixed numbers of days, 0 for a token that
// never expires.
const (
	maxTokenNameLength   = 255
	defaultTokenLifetime = 90
)

// tokenLifetimes are the lifetimes, in days, a token may be minted with.
var tokenLifetimes = []int{0, 30, 60, 90}

// tokenObject is the REST API's Token object.
type tokenObject struct {
	TokenID     string        `json:"token_id"`
	UserID      string        `json:"user_id"`
	Name        string        `json:"name"`
	Permissions []string      `json:"permissions"`
	Paths       []string      `json:"paths"`
	Workspace   *string       `json:"workspace"`
	ExpiresAt   *timefmt.Time `json:"expires_at"`
	RevokedAt   *timefmt.Time `json:"revoked_at"`
	CreatedAt   timefmt.Time  `json:"created_at"`
}

// object returns p as the Token object: null for the paths of a token
// without path limits, the workspace of an unbound token and the times it
// does not have.
func (p PAT) object() tokenObject {
	o := tokenObject{TokenID: p.TokenID, UserID: p.UserID, Name: p.Name, Permissions: p.Permissions, Paths: p.Paths,
		ExpiresAt: (*timefmt.Time)(p.ExpiresAt), RevokedAt: (*timefmt.Time)(p.RevokedAt), CreatedAt: timefmt.Time(p.CreatedAt)}
	if o.Permissions == nil {
		o.Permissions = []string{}
	}
	if p.Workspace != "" {
		o.Workspace = &p.Workspace
	}
	return o
}

// MarshalJSON writes p as the REST API's Token object. The token's value
// is not part of a PAT, so no Token object written from a record shows it.
func (p PAT) MarshalJSON() ([]byte, error) { return json.Marshal(p.object()) }

// createdToken is the answer to the request that minted a token: the Token
// object and the token's value, which no other answer carries.
type createdToken struct {
	tokenObject
	Token string `json:"token"`
}

// MintRequest is what a PAT is minted with.
type MintRequest struct {
	// Name is 1 to 255 characters.
	Name string
	// Permissions are registered scopes, at least one; a duplicate is
	// dropped and the order kept.
	Permissions []string
	// Paths are the token's path limits, validated with ValidatePaths; nil
	// for a token without path limits.
	Paths []string
	// Expires is the lifetime in days, 0, 30, 60 or 90, where 0 never
	// expires; nil means 90.
	Expires *int
	// ExpiresAt is the expiry instant a hub-internal caller sets in place
	// of Expires; it is kept to whole seconds. Expires must then be nil.
	ExpiresAt *time.Time
	// Workspace is the slug of the workspace the token is bound to, "" for
	// a token that is not bound. The caller has checked it.
	Workspace string
}

// MintPAT mints a PAT for the user of caller in tx, which it does not
// commit, so that a caller can mint a token with other changes of its own:
// it validates req, holds a PAT caller to the subset rule (CheckSubset),
// then stores the token with StorePAT, created now, and returns its value,
// which is shown once, with its record. It is the one way a PAT is created.
// A request that is not valid is answered 400 invalid_request, one beyond
// what a PAT caller holds 403 insufficient_scope, and neither stores
// anything. The token expires the given number of
// days after the whole second of now, or at req.ExpiresAt.
func MintPAT(ctx context.Context, tx *sql.Tx, caller Principal, req MintRequest, now time.Time) (string, PAT, error) {
	if caller.User == nil {
		return "", PAT{}, errors.New("a PAT is minted for a user, and the caller has none")
	}
	if req.Expires != nil && req.ExpiresAt != nil {
		return "", PAT{}, errors.New("a PAT is minted with a lifetime or an expiry instant, not both")
	}
	if n := utf8.RuneCountInString(req.Name); n == 0 || n > maxTokenNameLength {
		return "", PAT{}, server.InvalidRequest(fmt.Sprintf("name must be 1 to %d characters", maxTokenNameLength))
	}
	perms, err := tokenPermissions(req.Permissions)
	if err != nil {
		return "", PAT{}, err
	}
	paths, err := ValidatePaths(req.Paths)
	if err != nil {
		return "", PAT{}, server.InvalidRequest(err.Error())
	}
	expiresAt := req.ExpiresAt
	if expiresAt != nil {
		at := expiresAt.Truncate(time.Second)
		expiresAt = &at
	} else if expiresAt, err = tokenExpiry(req.Expires, now); err != nil {
		return "", PAT{}, err
	}
	// The subset rule needs a valid request, so it comes after validation,
	// and before anything is stored.
	if err := CheckSubset(caller, perms, paths); err != nil {
		return "", PAT{}, err
	}
	return StorePAT(ctx, tx, PATSpec{UserID: caller.User.ID, Name: req.Name, Permissions: perms, Paths: paths,
		Workspace: req.Workspace, ExpiresAt: expiresAt, CreatedAt: now})
}

// tokenPermissions checks that requested holds at least one scope and only
// registered ones, and returns it without duplicates, in its order.
func tokenPermissions(requested []string) ([]string, error) {
	if len(requested) == 0 {
		return nil, server.InvalidRequest("permissions must hold at least one scope")
	}
	perms := make([]string, 0, len(requested))
	for _, s := range requested {
		if !IsRegisteredScope(s) {
			return nil, server.InvalidRequest(fmt.Sprintf("permissions: %q is not a registered scope", s))
		}
		if !slices.Contains(perms, s) {
			perms = append(perms, s)
		}
	}
	return perms, nil
}

// tokenExpiry returns the expiry of a token minted now with the lifetime
// days, nil meaning the default: days × 24 hours after the whole second of
// now, or nil for 0, a token that never expires.
func tokenExpiry(days *int, now time.Time) (*time.Time, error) {
	d := defaultTokenLifetime
	if days != nil {
		d = *days
	}
	if !slices.Contains(tokenLifetimes, d) {
		return nil, server.InvalidRequest("expires must be one of 0, 30, 60 and 90")
	}
	if d == 0 {
		return nil, nil
	}
	at := now.Truncate(time.Second).Add(time.Duration(d) * 24 * time.Hour)
	return &at, nil
}
