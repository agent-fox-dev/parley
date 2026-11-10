// Package identity holds who may use the hub. This file is the admin
// settings: the bootstrap e-mail and the hash of the admin token, generated
// at first boot and verified when a request presents it.
package identity

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"errors"

	"github.com/agent-fox-dev/parley/internal/store"
)

// AdminTokenPrefix starts every admin token.
const AdminTokenPrefix = "parley_admin_"

// AdminSettingsStep creates the one-row table of admin settings.
var AdminSettingsStep = store.Step{
	Name:   "identity.admin_settings",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE admin_settings (
	id         INTEGER PRIMARY KEY CHECK (id = 1),
	email      TEXT NOT NULL,
	token_hash TEXT NOT NULL
)`)
		return err
	},
}

// Settings are the admin settings; Stored is false on a hub that has not
// been bootstrapped.
type Settings struct {
	Email     string
	TokenHash string
	Stored    bool
}

// AdminSettings reads the admin settings.
func AdminSettings(ctx context.Context, st *store.Store) (Settings, error) {
	var s Settings
	err := st.DB().QueryRowContext(ctx, `SELECT email, token_hash FROM admin_settings WHERE id = 1`).Scan(&s.Email, &s.TokenHash)
	if errors.Is(err, sql.ErrNoRows) {
		return Settings{}, nil
	}
	if err != nil {
		return Settings{}, err
	}
	s.Stored = true
	return s, nil
}

// SetAdmin stores the hash of a new admin token, replacing the one stored,
// and the e-mail when email is not empty; an empty email keeps the stored
// one.
func SetAdmin(ctx context.Context, tx *sql.Tx, email, tokenHash string) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO admin_settings (id, email, token_hash) VALUES (1, ?, ?)
ON CONFLICT (id) DO UPDATE SET token_hash = excluded.token_hash,
	email = CASE WHEN excluded.email = '' THEN admin_settings.email ELSE excluded.email END`,
		email, tokenHash)
	return err
}

// GenerateAdminToken returns a new admin token: parley_admin_ followed by 64
// lowercase hexadecimal characters from 32 bytes of the system's random
// source.
func GenerateAdminToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return AdminTokenPrefix + hex.EncodeToString(b[:]), nil
}

// HashToken is the stored form of a token: the lowercase hexadecimal SHA-256
// of the whole token.
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// VerifyAdminToken reports whether presented is the admin token whose hash
// is stored, comparing in constant time; false when none is stored.
func VerifyAdminToken(ctx context.Context, st *store.Store, presented string) (bool, error) {
	s, err := AdminSettings(ctx, st)
	if err != nil || !s.Stored {
		return false, err
	}
	return subtle.ConstantTimeCompare([]byte(HashToken(presented)), []byte(s.TokenHash)) == 1, nil
}
