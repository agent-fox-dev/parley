package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
	for _, s := range requested {
		if !IsRegisteredScope(s) {
			return nil, server.InvalidRequest(fmt.Sprintf("permissions: %q is not a registered scope", s))
		}
	}
	return appendNew(make([]string, 0, len(requested)), requested...), nil
}

// appendNew appends to perms each of scopes it does not hold yet, in order.
func appendNew(perms []string, scopes ...string) []string {
	for _, s := range scopes {
		if !slices.Contains(perms, s) {
			perms = append(perms, s)
		}
	}
	return perms
}

// changedPermissions returns the permissions current become under the
// permission change method with the validated list given: PUT the list
// given without duplicates, PATCH current followed by the scopes of given
// it does not hold, in their order, and DELETE current without the scopes
// given. The result is never nil, so that an empty list is stored as [].
func changedPermissions(method string, current, given []string) []string {
	switch method {
	case http.MethodPut:
		return appendNew([]string{}, given...)
	case http.MethodPatch:
		return appendNew(slices.Clone(current), given...)
	default:
		return slices.DeleteFunc(append([]string{}, current...), func(s string) bool { return slices.Contains(given, s) })
	}
}

// setTokenPermissions stores perms as the permissions of the PAT tokenID in
// tx and returns its record as stored then. An empty perms also revokes the
// token at now, in the same statement, since a token without a scope can do
// nothing and must not stay live.
func setTokenPermissions(ctx context.Context, tx *sql.Tx, tokenID string, perms []string, now time.Time) (PAT, error) {
	data, err := json.Marshal(perms)
	if err != nil {
		return PAT{}, fmt.Errorf("encode a PAT's permissions: %w", err)
	}
	query, args := `UPDATE pats SET permissions = ? WHERE token_id = ?`, []any{string(data), tokenID}
	if len(perms) == 0 {
		query, args = `UPDATE pats SET permissions = ?, revoked_at = ? WHERE token_id = ?`, []any{string(data), now.UTC().UnixNano(), tokenID}
	}
	if _, err := tx.ExecContext(ctx, query, args...); err != nil {
		return PAT{}, fmt.Errorf("set the permissions of PAT %s: %w", tokenID, err)
	}
	return PATByID(ctx, tx, tokenID)
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

// errTokenNotFound is the answer for a token_id the caller's user does not
// own, whether it exists or not.
func errTokenNotFound() error { return server.NotFound("token not found") }

// tokenOfUser returns the PAT tokenID when the user userID owns it. A token
// that does not exist and one of another user are both errTokenNotFound,
// so that the answer tells nothing of other users' tokens.
func tokenOfUser(ctx context.Context, q Querier, userID, tokenID string) (PAT, error) {
	p, err := PATByID(ctx, q, tokenID)
	if errors.Is(err, ErrNotFound) || err == nil && p.UserID != userID {
		return PAT{}, errTokenNotFound()
	}
	return p, err
}

// tokenPosition is the position of a token in a list of tokens: newest
// first, created_at descending, then token_id descending, so that tokens
// created at the same instant keep one order across pages.
type tokenPosition struct {
	CreatedAt int64  `json:"c"`
	ID        string `json:"id"`
}

// position returns the position of p in a list of tokens.
func (p PAT) position() any { return tokenPosition{CreatedAt: p.CreatedAt.UnixNano(), ID: p.TokenID} }

// listTokens returns up to limit PATs of the user userID in any state, bound
// ones included, in the order of a list of tokens, after the position after
// when it is not nil.
func listTokens(ctx context.Context, q RowsQuerier, userID string, after *tokenPosition, limit int) ([]PAT, error) {
	query := `SELECT ` + patColumns + ` FROM pats WHERE user_id = ?`
	args := []any{userID}
	if after != nil {
		query += ` AND (created_at < ? OR created_at = ? AND token_id < ?)`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	query += ` ORDER BY created_at DESC, token_id DESC LIMIT ?`
	rows, err := q.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list the PATs: %w", err)
	}
	defer rows.Close()
	var pats []PAT
	for rows.Next() {
		p, err := scanPAT(rows)
		if err != nil {
			return nil, fmt.Errorf("list the PATs: %w", err)
		}
		pats = append(pats, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the PATs: %w", err)
	}
	return pats, nil
}

// revokeTokenOfUser revokes the PAT tokenID of the user userID in tx, as
// RevokeTokenByID does, and returns its record as stored then; a token the
// user does not own is errTokenNotFound.
func revokeTokenOfUser(ctx context.Context, tx *sql.Tx, userID, tokenID string, now time.Time) (PAT, error) {
	if _, err := tokenOfUser(ctx, tx, userID, tokenID); err != nil {
		return PAT{}, err
	}
	if err := RevokeTokenByID(ctx, tx, tokenID, now); err != nil {
		return PAT{}, err
	}
	return PATByID(ctx, tx, tokenID)
}

// RevokeTokenByID sets the revoked_at of the PAT tokenID to now in tx, which
// it does not commit, so that a hub-internal caller revokes a token with
// other changes of its own. A token already revoked keeps its revoked_at,
// and an expired one is revoked as a live one is. It checks no owner and
// writes no event; a token that does not exist is ErrNotFound.
func RevokeTokenByID(ctx context.Context, tx *sql.Tx, tokenID string, now time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE pats SET revoked_at = ? WHERE token_id = ? AND revoked_at IS NULL`,
		now.UTC().UnixNano(), tokenID)
	if err != nil {
		return fmt.Errorf("revoke PAT %s: %w", tokenID, err)
	}
	if n, err := res.RowsAffected(); err != nil || n > 0 {
		return err
	}
	found, err := exists(ctx, tx, `SELECT count(*) FROM pats WHERE token_id = ?`, tokenID)
	if err == nil && !found {
		err = ErrNotFound
	}
	return err
}

// SetTokenExpiryByID sets the expires_at of the PAT tokenID to at, kept to
// whole seconds, in tx, which it does not commit, and changes nothing else
// of the token, revoked_at included. It checks no owner and writes no
// event; a token that does not exist is ErrNotFound.
func SetTokenExpiryByID(ctx context.Context, tx *sql.Tx, tokenID string, at time.Time) error {
	res, err := tx.ExecContext(ctx, `UPDATE pats SET expires_at = ? WHERE token_id = ?`,
		at.Truncate(time.Second).UTC().UnixNano(), tokenID)
	if err != nil {
		return fmt.Errorf("set the expiry of PAT %s: %w", tokenID, err)
	}
	n, err := res.RowsAffected()
	if err == nil && n == 0 {
		err = ErrNotFound
	}
	return err
}
