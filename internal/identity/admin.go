// Package identity holds who may use the hub: the admin token that a new
// hub hands its operator, the users and their orgs, the API keys and the
// personal access tokens (PATs), and the authenticator that tells, for every
// request to a non-public route, which of them is calling. The hub stores
// only the hash of a secret: the admin token exists only in the file the
// operator is told to delete, and an API key or PAT only in the answer that
// issued it.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/agent-fox-dev/parley/internal/store"
)

// AdminTokenPrefix begins every admin token, so that the scrubber and the
// operator recognise one by its shape.
const AdminTokenPrefix = "parley_admin_"

// adminTokenBytes is how many random bytes an admin token carries.
const adminTokenBytes = 32

// AdminSettingsStep creates the admin settings in the transactional store:
// one row, which exists once a hub is bootstrapped, holding the bootstrap
// e-mail (empty when none was given) and the hash of the admin token.
var AdminSettingsStep = store.Step{
	Name:   "admin_settings",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE admin_settings (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	email      TEXT    NOT NULL,
	token_hash TEXT    NOT NULL
)`)
		return err
	},
}

// Admin is what the hub stores about its admin. The zero Admin means that
// none is stored: the hub is not bootstrapped.
type Admin struct {
	// Email is the address given when the hub was bootstrapped, or "".
	Email string
	// TokenHash is HashAdminToken of the admin token.
	TokenHash string
}

// Stored reports whether an admin token hash is stored, that is, whether
// the hub is bootstrapped.
func (a Admin) Stored() bool { return a.TokenHash != "" }

// AdminSettings returns the stored admin settings, the zero Admin when none
// is stored.
func AdminSettings(ctx context.Context, st *store.Store) (Admin, error) {
	var a Admin
	err := st.Main().QueryRowContext(ctx, `SELECT email, token_hash FROM admin_settings WHERE id = 1`).
		Scan(&a.Email, &a.TokenHash)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return Admin{}, nil
	case err != nil:
		return Admin{}, fmt.Errorf("read the admin settings: %w", err)
	}
	return a, nil
}

// GenerateAdminToken returns a new admin token: AdminTokenPrefix followed by
// 32 bytes of the system's random source as 64 lowercase hex characters.
func GenerateAdminToken() (string, error) {
	b := make([]byte, adminTokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate an admin token: %w", err)
	}
	return AdminTokenPrefix + hex.EncodeToString(b), nil
}

// HashAdminToken returns the hash the hub stores for token: the lowercase
// hex SHA-256 of the whole token, prefix included.
func HashAdminToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyAdminToken reports whether token is the admin token, by comparing
// its hash with the stored one in constant time. It reports false when no
// hash is stored.
func VerifyAdminToken(ctx context.Context, st *store.Store, token string) (bool, error) {
	a, err := AdminSettings(ctx, st)
	if err != nil || !a.Stored() {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(HashAdminToken(token)), []byte(a.TokenHash)) == 1, nil
}

// IssueAdminToken generates a new admin token and replaces the stored hash
// with its hash; when no hash was stored before it stores email as well. It
// hands the token to deliver inside the transaction that stores the hash, so
// that a token deliver cannot hand on is stored nowhere: an error from
// deliver rolls the transaction back and is returned as it is. The token
// leaves IssueAdminToken only through deliver.
func IssueAdminToken(ctx context.Context, st *store.Store, email string, deliver func(token string) error) error {
	token, err := GenerateAdminToken()
	if err != nil {
		return err
	}
	return st.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO admin_settings (id, email, token_hash) VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET token_hash = excluded.token_hash`, email, HashAdminToken(token))
		if err != nil {
			return fmt.Errorf("store the admin token hash: %w", err)
		}
		return deliver(token)
	})
}
