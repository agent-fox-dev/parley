package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// keyRoutes are the routes of the caller's own API keys.
func (s *Service) keyRoutes() []server.Route {
	return []server.Route{
		{Method: http.MethodGet, Pattern: "/user/keys", Scope: "keys:read", Marks: needsUserMark, Handler: s.listOwnKeys},
		// No scope, since only an API key may refresh, and not a create
		// route: a refresh makes no resource, so an Idempotency-Key is
		// ignored rather than replayed.
		{Method: http.MethodPost, Pattern: "/user/keys/:key_id/refresh", Marks: needsUserMark, Handler: s.refreshOwnKey},
		{Method: http.MethodDelete, Pattern: "/user/keys/:key_id", Scope: "keys:manage", Marks: needsUserMark,
			Handler: s.revokeOwnKey},
	}
}

// listOwnKeys answers GET /user/keys: a page of the caller's API keys.
func (s *Service) listOwnKeys(w http.ResponseWriter, r *http.Request) error {
	caller, err := callerUser(r)
	if err != nil {
		return err
	}
	return s.writeKeys(w, r, caller.ID)
}

// writeKeys answers a page of the API keys of the user userID in any
// state, newest first: created_at descending, then key_id descending.
func (s *Service) writeKeys(w http.ResponseWriter, r *http.Request, userID string) error {
	return keyTable.writePage(w, r, s.deps.Store.Main(), userID)
}

// refreshOwnKey answers POST /user/keys/:key_id/refresh: it gives the
// caller's live API key a new secret and a new lifetime of its
// expires_days from now, keeping its key_id, and answers 200 with the new
// key, shown this once. Only an API key may refresh, so that a PAT, which
// a person hands to automation, cannot take over the person's key; a PAT
// is refused before the key is looked up. A revoked or expired key is 409
// and unchanged. The old secret stops authenticating at once, since
// authentication reads the stored hash on every request.
func (s *Service) refreshOwnKey(w http.ResponseWriter, r *http.Request) error {
	caller, ok := PrincipalOf(r.Context())
	if !ok || caller.User == nil {
		return errors.New("a route that needs a user was reached without one")
	}
	if caller.Type != TypeAPIKey {
		return server.Forbidden("this route needs an API key")
	}
	now := s.deps.Now()
	var answer issuedKey
	err := s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		k, err := keyTable.ofUser(r.Context(), tx, caller.User.ID, server.Param(r, "key_id"))
		if err != nil {
			return err
		}
		if k.RevokedAt != nil {
			return server.Conflict("API key is revoked")
		}
		if expired(k.ExpiresAt, now) {
			return server.Conflict("API key is expired")
		}
		secret, err := randomString(apiKeyAlphabet, secretLength)
		if err != nil {
			return err
		}
		expiresAt := keyExpiry(k.ExpiresDays, now)
		if err := setKeySecret(r.Context(), tx, k.KeyID, hashSecret(secret), expiresAt); err != nil {
			return err
		}
		answer = issuedKey{Key: apiKeyPrefix + k.KeyID + "_" + secret, KeyID: k.KeyID, ExpiresAt: (*timefmt.Time)(expiresAt)}
		return nil
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, answer)
	return nil
}

// revokeOwnKey answers DELETE /user/keys/:key_id: it revokes the caller's
// API key now, also an expired one and the one the request came with, and
// answers 200 with the key; a revoked key is answered as it is.
func (s *Service) revokeOwnKey(w http.ResponseWriter, r *http.Request) error {
	caller, err := callerUser(r)
	if err != nil {
		return err
	}
	return s.revokeKey(w, r, caller.ID)
}

// revokeKey revokes the API key of the user userID the path's key_id names
// and answers 200 with it.
func (s *Service) revokeKey(w http.ResponseWriter, r *http.Request, userID string) error {
	var k APIKey
	err := s.deps.Store.Tx(r.Context(), func(tx *sql.Tx) error {
		var err error
		k, err = revokeKeyOfUser(r.Context(), tx, userID, server.Param(r, "key_id"), s.deps.Now())
		return err
	})
	if err != nil {
		return err
	}
	server.WriteJSON(w, http.StatusOK, k)
	return nil
}

// errKeyNotFound is the answer for a key_id the caller's user does not
// own, whether it exists or not.
func errKeyNotFound() error { return server.NotFound("API key not found") }

// keyExpiry returns the expiry of a key refreshed now with the lifetime
// days: days × 24 hours after the whole second of now, or nil for 0, a key
// that never expires.
func keyExpiry(days int, now time.Time) *time.Time {
	if days == 0 {
		return nil
	}
	at := now.UTC().Truncate(time.Second).Add(time.Duration(days) * 24 * time.Hour)
	return &at
}

// setKeySecret stores hash as the secret hash of the API key keyID and
// expiresAt as its expiry, in tx.
func setKeySecret(ctx context.Context, tx *sql.Tx, keyID, hash string, expiresAt *time.Time) error {
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET secret_hash = ?, expires_at = ? WHERE key_id = ?`,
		hash, nanos(expiresAt), keyID); err != nil {
		return fmt.Errorf("refresh API key %s: %w", keyID, err)
	}
	return nil
}

// revokeKeyOfUser sets the revoked_at of the API key keyID of the user
// userID to now in tx when it is not revoked yet, so that a revoked key
// keeps its first revoked_at, and returns its record as stored then; a key
// the user does not own is errKeyNotFound.
func revokeKeyOfUser(ctx context.Context, tx *sql.Tx, userID, keyID string, now time.Time) (APIKey, error) {
	if _, err := keyTable.ofUser(ctx, tx, userID, keyID); err != nil {
		return APIKey{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ? WHERE key_id = ? AND revoked_at IS NULL`,
		now.UTC().UnixNano(), keyID); err != nil {
		return APIKey{}, fmt.Errorf("revoke API key %s: %w", keyID, err)
	}
	return APIKeyByID(ctx, tx, keyID)
}
