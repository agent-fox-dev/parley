package identity

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// Querier is what reads run on: the store's database or an open
// transaction.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// User is a person known to the hub.
type User struct {
	ID         string       `json:"id"`
	Username   string       `json:"username"`
	Email      string       `json:"email"`
	FullName   string       `json:"full_name"`
	Role       string       `json:"role"`
	Status     string       `json:"status"`
	Provider   string       `json:"provider"`
	ProviderID string       `json:"provider_id"`
	CreatedAt  timefmt.Time `json:"created_at"`
	UpdatedAt  timefmt.Time `json:"updated_at"`
}

// Roles and statuses of users.
const (
	RoleAdmin     = "admin"
	RoleUser      = "user"
	StatusActive  = "active"
	StatusBlocked = "blocked"
)

// ErrNotFound is returned by a lookup that finds no record.
var ErrNotFound = errors.New("not found")

func nanos(t time.Time) int64 { return t.UTC().UnixNano() }

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

func nullNanos(t *time.Time) any {
	if t == nil {
		return nil
	}
	return nanos(*t)
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// NewID returns a random version 4 UUID.
func NewID() string {
	var b [16]byte
	_, _ = rand.Read(b[:]) // crypto/rand.Read never fails.
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

const userColumns = `id, username, email, full_name, role, status, COALESCE(provider, ''), COALESCE(provider_id, ''), created_at, updated_at`

func scanUser(row interface{ Scan(...any) error }) (User, error) {
	var u User
	var created, updated int64
	err := row.Scan(&u.ID, &u.Username, &u.Email, &u.FullName, &u.Role, &u.Status, &u.Provider, &u.ProviderID, &created, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return User{}, ErrNotFound
	}
	if err != nil {
		return User{}, err
	}
	u.CreatedAt, u.UpdatedAt = timefmt.Time(fromNanos(created)), timefmt.Time(fromNanos(updated))
	return u, nil
}

// GetUser reads a user by id.
func GetUser(ctx context.Context, q Querier, id string) (User, error) {
	return scanUser(q.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE id = ?`, id))
}

// CreateUser stores a user with a new id and created_at and updated_at at
// now. A taken username, e-mail or provider identity is a database
// constraint error.
func CreateUser(ctx context.Context, tx *sql.Tx, u User, now time.Time) (User, error) {
	u.ID = NewID()
	u.CreatedAt, u.UpdatedAt = timefmt.Time(now.UTC()), timefmt.Time(now.UTC())
	_, err := tx.ExecContext(ctx, `INSERT INTO users (id, username, email, full_name, role, status, provider, provider_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		u.ID, u.Username, u.Email, u.FullName, u.Role, u.Status, nullString(u.Provider), nullString(u.ProviderID), nanos(now), nanos(now))
	if err != nil {
		return User{}, err
	}
	return u, nil
}

// IssueAPIKey generates an API key for a user, stores the hash of its secret
// with a lifetime of days (0: it never expires) and returns the whole key
// once, its key_id and its expires_at.
func IssueAPIKey(ctx context.Context, tx *sql.Tx, userID string, days int, now time.Time) (key, keyID string, expiresAt *time.Time, err error) {
	if days > 0 {
		at := now.UTC().Add(time.Duration(days) * 24 * time.Hour)
		expiresAt = &at
	}
	for {
		id, err := randomString(alnum, 8)
		if err != nil {
			return "", "", nil, err
		}
		secret, err := randomString(alnum, 32)
		if err != nil {
			return "", "", nil, err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO api_keys (key_id, user_id, secret_hash, expires_days, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?)`, id, userID, HashSecret(secret), days, nullNanos(expiresAt), nanos(now))
		if err != nil {
			return "", "", nil, err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return "parley_" + id + "_" + secret, id, expiresAt, nil
		}
	}
}

// PATSpec describes a personal access token to store.
type PATSpec struct {
	Name        string
	Permissions []string
	// Paths are the globs a token's writes are limited to; nil for none.
	Paths []string
	// Workspace is the workspace the token is bound to; "" for none.
	Workspace string
	ExpiresAt *time.Time
}

// StorePAT generates a personal access token for a user and stores the hash
// of its secret with the spec, inside the transaction it is given,
// generating the id again when it exists. It validates nothing beyond the
// record's shape: package 06's minting operation validates and then calls
// it; no other production code does. It returns the whole token once and
// its token_id.
func StorePAT(ctx context.Context, tx *sql.Tx, userID string, spec PATSpec, now time.Time) (token, tokenID string, err error) {
	perms := spec.Permissions
	if perms == nil {
		perms = []string{}
	}
	permJSON, err := json.Marshal(perms)
	if err != nil {
		return "", "", err
	}
	var pathsJSON any
	if spec.Paths != nil {
		b, err := json.Marshal(spec.Paths)
		if err != nil {
			return "", "", err
		}
		pathsJSON = string(b)
	}
	for {
		id, err := randomString(lowerAlnum, 8)
		if err != nil {
			return "", "", err
		}
		secret, err := randomString(lowerAlnum, 32)
		if err != nil {
			return "", "", err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO pats (token_id, user_id, secret_hash, name, permissions, paths, workspace, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, id, userID, HashSecret(secret), spec.Name, string(permJSON), pathsJSON,
			nullString(spec.Workspace), nullNanos(spec.ExpiresAt), nanos(now))
		if err != nil {
			return "", "", err
		}
		if n, _ := res.RowsAffected(); n == 1 {
			return "parley_pat_" + id + "_" + secret, id, nil
		}
	}
}

// storedCredential is an API key or PAT as authentication reads it.
type storedCredential struct {
	userID      string
	secretHash  string
	expiresAt   *time.Time
	revokedAt   *time.Time
	permissions []string
	paths       []string
	workspace   *string
}

func nullTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromNanos(n.Int64)
	return &t
}

// lookupCredential reads the API key or PAT with the given id.
func lookupCredential(ctx context.Context, q Querier, kind, id string) (storedCredential, error) {
	var c storedCredential
	var expires, revoked sql.NullInt64
	if kind == TypeAPIKey {
		err := q.QueryRowContext(ctx, `SELECT user_id, secret_hash, expires_at, revoked_at FROM api_keys WHERE key_id = ?`, id).
			Scan(&c.userID, &c.secretHash, &expires, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return c, ErrNotFound
		}
		if err != nil {
			return c, err
		}
	} else {
		var perms string
		var paths, workspace sql.NullString
		err := q.QueryRowContext(ctx, `SELECT user_id, secret_hash, permissions, paths, workspace, expires_at, revoked_at FROM pats WHERE token_id = ?`, id).
			Scan(&c.userID, &c.secretHash, &perms, &paths, &workspace, &expires, &revoked)
		if errors.Is(err, sql.ErrNoRows) {
			return c, ErrNotFound
		}
		if err != nil {
			return c, err
		}
		if err := json.Unmarshal([]byte(perms), &c.permissions); err != nil {
			return c, err
		}
		if paths.Valid {
			if err := json.Unmarshal([]byte(paths.String), &c.paths); err != nil {
				return c, err
			}
		}
		if workspace.Valid {
			ws := workspace.String
			c.workspace = &ws
		}
	}
	c.expiresAt, c.revokedAt = nullTime(expires), nullTime(revoked)
	return c, nil
}
