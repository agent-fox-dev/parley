package identity

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"time"
	"uuid"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/agent-fox-dev/parley/internal/server/timefmt"
)

// ErrNotFound is returned by a lookup whose record does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is what an error of CreateUserAndPersonalOrg is, errors.Is,
// when the user or the personal org would break a uniqueness rule. Its
// Error text says which rule, in fixed words.
var ErrConflict = errors.New("conflict")

// conflictError is an ErrConflict with the rule it breaks.
type conflictError struct{ msg string }

func (e conflictError) Error() string        { return e.msg }
func (e conflictError) Is(target error) bool { return target == ErrConflict }

// The roles and statuses of a user.
const (
	RoleUser      = "user"
	RoleAdmin     = "admin"
	StatusActive  = "active"
	StatusBlocked = "blocked"
)

// User is a person who signs in through an identity provider. It marshals
// to JSON as the REST API's User object.
type User struct {
	ID         string
	Username   string
	Email      string
	FullName   string
	Role       string
	Status     string
	Provider   string
	ProviderID string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

// MarshalJSON writes u as the REST API's User object.
func (u User) MarshalJSON() ([]byte, error) {
	return json.Marshal(struct {
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
	}{u.ID, u.Username, u.Email, u.FullName, u.Role, u.Status, u.Provider, u.ProviderID,
		timefmt.Time(u.CreatedAt), timefmt.Time(u.UpdatedAt)})
}

// Org is an organization. A user's personal org is named after the user,
// who owns it and is its first member.
type Org struct {
	ID   string
	Name string
	Slug string
	URL  string
	// OwnerID is the id of the owning user, "" when it has none.
	OwnerID   string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewUser is a user CreateUserAndPersonalOrg creates.
type NewUser struct {
	Username   string
	Email      string
	FullName   string
	Provider   string
	ProviderID string
	// CreatedAt is the creation time of the user, the org and the
	// membership.
	CreatedAt time.Time
}

// APIKey is the stored record of an API key; its secret is not part of it.
type APIKey struct {
	KeyID  string
	UserID string
	// ExpiresDays is the lifetime the key was issued with; 0 means it never
	// expires.
	ExpiresDays int
	// ExpiresAt is nil when the key never expires.
	ExpiresAt *time.Time
	// RevokedAt is nil while the key is not revoked.
	RevokedAt *time.Time
	CreatedAt time.Time

	hash string
}

// PAT is the stored record of a personal access token; its secret is not
// part of it.
type PAT struct {
	TokenID     string
	UserID      string
	Name        string
	Permissions []string
	// Paths is nil when the token has no path limits.
	Paths []string
	// Workspace is the slug the token is bound to, "" when it is not bound.
	Workspace string
	// ExpiresAt is nil when the token never expires.
	ExpiresAt *time.Time
	// RevokedAt is nil while the token is not revoked.
	RevokedAt *time.Time
	CreatedAt time.Time

	hash string
}

// PATSpec is the record StorePAT stores, without its id.
type PATSpec struct {
	UserID      string
	Name        string
	Permissions []string
	// Paths is nil for a token without path limits.
	Paths []string
	// Workspace is "" for a token that is not bound.
	Workspace string
	// ExpiresAt is nil for a token that never expires.
	ExpiresAt *time.Time
	CreatedAt time.Time
}

// Querier is what a lookup reads through: the store's *sql.DB or an open
// *sql.Tx.
type Querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// idAttempts is how many ids a write draws before it gives up; with 62^8 or
// 36^8 ids, a second draw is already rare.
const idAttempts = 5

// freshID draws a credential until its id is not in column of table.
func freshID(ctx context.Context, tx *sql.Tx, table, column, prefix, alphabet string) (credential, id, secret string, err error) {
	for range idAttempts {
		if credential, id, secret, err = newCredential(prefix, alphabet); err != nil {
			return "", "", "", err
		}
		var n int
		err = tx.QueryRowContext(ctx, `SELECT count(*) FROM `+table+` WHERE `+column+` = ?`, id).Scan(&n)
		if err != nil {
			return "", "", "", fmt.Errorf("look up a new %s: %w", column, err)
		}
		if n == 0 {
			return credential, id, secret, nil
		}
	}
	return "", "", "", fmt.Errorf("no free %s after %d attempts", column, idAttempts)
}

// IssueAPIKey generates an API key for the user userID, stores its record
// in tx with the hash of its secret, and returns the key, which is shown
// once, with its record. The key expires expiresDays × 24 hours after now;
// with expiresDays 0 it never expires.
func IssueAPIKey(ctx context.Context, tx *sql.Tx, userID string, expiresDays int, now time.Time) (string, APIKey, error) {
	key, id, secret, err := freshID(ctx, tx, "api_keys", "key_id", apiKeyPrefix, apiKeyAlphabet)
	if err != nil {
		return "", APIKey{}, err
	}
	rec := APIKey{KeyID: id, UserID: userID, ExpiresDays: expiresDays, CreatedAt: now.UTC(), hash: hashSecret(secret)}
	if expiresDays > 0 {
		at := rec.CreatedAt.Add(time.Duration(expiresDays) * 24 * time.Hour)
		rec.ExpiresAt = &at
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO api_keys (key_id, user_id, secret_hash, expires_days, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?)`, rec.KeyID, rec.UserID, rec.hash, rec.ExpiresDays, nanos(rec.ExpiresAt), rec.CreatedAt.UnixNano())
	if err != nil {
		return "", APIKey{}, fmt.Errorf("store an API key: %w", err)
	}
	return key, rec, nil
}

// revokeLiveAPIKeys sets revoked_at to now on every API key of the user
// userID that is neither revoked nor expired at now. A key that is already
// revoked keeps its revocation time, and an expired key is left as it is.
func revokeLiveAPIKeys(ctx context.Context, tx *sql.Tx, userID string, now time.Time) error {
	at := now.UnixNano()
	_, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ?
WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`, at, userID, at)
	if err != nil {
		return fmt.Errorf("revoke the API keys of a user: %w", err)
	}
	return nil
}

// updateUserLogin stores the username, the e-mail and updated_at of u, what
// a later login refreshes.
func updateUserLogin(ctx context.Context, tx *sql.Tx, u User) error {
	_, err := tx.ExecContext(ctx, `UPDATE users SET username = ?, email = ?, updated_at = ? WHERE id = ?`,
		u.Username, u.Email, u.UpdatedAt.UnixNano(), u.ID)
	if isUniqueViolation(err) {
		return conflictError{"the username or the e-mail is taken"}
	}
	if err != nil {
		return fmt.Errorf("store the user: %w", err)
	}
	return nil
}

// StorePAT generates a PAT with the fields of spec, stores its record in tx
// with the hash of its secret, and returns the token, which is shown once,
// with its record. It draws the id again when it is taken and checks
// nothing else: the caller has validated spec.
func StorePAT(ctx context.Context, tx *sql.Tx, spec PATSpec) (string, PAT, error) {
	token, id, secret, err := freshID(ctx, tx, "pats", "token_id", patPrefix, patAlphabet)
	if err != nil {
		return "", PAT{}, err
	}
	rec := PAT{
		TokenID: id, UserID: spec.UserID, Name: spec.Name, Permissions: spec.Permissions, Paths: spec.Paths,
		Workspace: spec.Workspace, ExpiresAt: utc(spec.ExpiresAt), CreatedAt: spec.CreatedAt.UTC(), hash: hashSecret(secret),
	}
	if rec.Permissions == nil {
		rec.Permissions = []string{}
	}
	perms, err := json.Marshal(rec.Permissions)
	if err != nil {
		return "", PAT{}, fmt.Errorf("encode a PAT's permissions: %w", err)
	}
	var paths, workspace sql.NullString
	if rec.Paths != nil {
		b, err := json.Marshal(rec.Paths)
		if err != nil {
			return "", PAT{}, fmt.Errorf("encode a PAT's paths: %w", err)
		}
		paths = sql.NullString{String: string(b), Valid: true}
	}
	if rec.Workspace != "" {
		workspace = sql.NullString{String: rec.Workspace, Valid: true}
	}
	_, err = tx.ExecContext(ctx, `INSERT INTO pats (token_id, user_id, secret_hash, name, permissions, paths, workspace, expires_at, created_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`, rec.TokenID, rec.UserID, rec.hash, rec.Name, string(perms), paths, workspace,
		nanos(rec.ExpiresAt), rec.CreatedAt.UnixNano())
	if err != nil {
		return "", PAT{}, fmt.Errorf("store a PAT: %w", err)
	}
	return token, rec, nil
}

// UserByID returns the user with the id id, or ErrNotFound.
func UserByID(ctx context.Context, q Querier, id string) (User, error) {
	return userWhere(ctx, q, `id = ?`, id)
}

// UserByProvider returns the user who signs in as providerID at the
// identity provider provider, or ErrNotFound.
func UserByProvider(ctx context.Context, q Querier, provider, providerID string) (User, error) {
	return userWhere(ctx, q, `provider = ? AND provider_id = ?`, provider, providerID)
}

// userWhere returns the user the condition where, with the arguments args,
// selects.
func userWhere(ctx context.Context, q Querier, where string, args ...any) (User, error) {
	var u User
	var created, updated int64
	err := q.QueryRowContext(ctx, `SELECT id, username, email, full_name, role, status, provider, provider_id, created_at, updated_at
FROM users WHERE `+where, args...).Scan(&u.ID, &u.Username, &u.Email, &u.FullName, &u.Role, &u.Status, &u.Provider,
		&u.ProviderID, &created, &updated)
	if err != nil {
		return User{}, lookupErr("user", err)
	}
	u.CreatedAt, u.UpdatedAt = fromNanos(created), fromNanos(updated)
	return u, nil
}

// APIKeyByID returns the record of the API key with the id keyID, or
// ErrNotFound.
func APIKeyByID(ctx context.Context, q Querier, keyID string) (APIKey, error) {
	var k APIKey
	var expires, revoked sql.NullInt64
	var created int64
	err := q.QueryRowContext(ctx, `SELECT key_id, user_id, secret_hash, expires_days, expires_at, revoked_at, created_at
FROM api_keys WHERE key_id = ?`, keyID).Scan(&k.KeyID, &k.UserID, &k.hash, &k.ExpiresDays, &expires, &revoked, &created)
	if err != nil {
		return APIKey{}, lookupErr("API key", err)
	}
	k.ExpiresAt, k.RevokedAt, k.CreatedAt = nullTime(expires), nullTime(revoked), fromNanos(created)
	return k, nil
}

// PATByID returns the record of the PAT with the id tokenID, or
// ErrNotFound.
func PATByID(ctx context.Context, q Querier, tokenID string) (PAT, error) {
	var p PAT
	var perms string
	var paths, workspace sql.NullString
	var expires, revoked sql.NullInt64
	var created int64
	err := q.QueryRowContext(ctx, `SELECT token_id, user_id, secret_hash, name, permissions, paths, workspace, expires_at, revoked_at, created_at
FROM pats WHERE token_id = ?`, tokenID).Scan(&p.TokenID, &p.UserID, &p.hash, &p.Name, &perms, &paths, &workspace,
		&expires, &revoked, &created)
	if err != nil {
		return PAT{}, lookupErr("PAT", err)
	}
	if err := json.Unmarshal([]byte(perms), &p.Permissions); err != nil {
		return PAT{}, fmt.Errorf("decode the permissions of PAT %s: %w", tokenID, err)
	}
	if paths.Valid {
		if err := json.Unmarshal([]byte(paths.String), &p.Paths); err != nil {
			return PAT{}, fmt.Errorf("decode the paths of PAT %s: %w", tokenID, err)
		}
	}
	p.Workspace = workspace.String
	p.ExpiresAt, p.RevokedAt, p.CreatedAt = nullTime(expires), nullTime(revoked), fromNanos(created)
	return p, nil
}

func lookupErr(what string, err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	return fmt.Errorf("read %s: %w", what, err)
}

func fromNanos(n int64) time.Time { return time.Unix(0, n).UTC() }

func nullTime(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := fromNanos(n.Int64)
	return &t
}

func nanos(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixNano(), Valid: true}
}

func utc(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	u := t.UTC()
	return &u
}

// slugSuffixes is how many numbered slugs, <slug>-1 to <slug>-10, a
// personal org tries after its derived one.
const slugSuffixes = 10

// CreateUserAndPersonalOrg creates, in tx, the active user nu with the role
// role, the user's personal org and the user's membership of it. The org is
// active, named after the username and owned by the user; its slug is
// derived from the username (see slug), or the first free of <slug>-1 to
// <slug>-10 when that one is taken. It returns an error that is
// ErrConflict, and writes nothing, when the username, the e-mail or the
// provider identity is another user's, an org has the username as its name
// or no slug is free. The caller picks the username and the role. Names
// compare as stored, letter case included.
func CreateUserAndPersonalOrg(ctx context.Context, tx *sql.Tx, nu NewUser, role string) (User, Org, error) {
	for _, c := range []struct {
		query string
		args  []any
		msg   string
	}{
		{`SELECT count(*) FROM users WHERE username = ?`, []any{nu.Username}, "the username is taken"},
		{`SELECT count(*) FROM users WHERE email = ?`, []any{nu.Email}, "the e-mail belongs to another user"},
		{`SELECT count(*) FROM users WHERE provider = ? AND provider_id = ?`, []any{nu.Provider, nu.ProviderID},
			"the identity belongs to another user"},
		{`SELECT count(*) FROM orgs WHERE name = ?`, []any{nu.Username}, "an org has the username as its name"},
	} {
		taken, err := exists(ctx, tx, c.query, c.args...)
		if err != nil {
			return User{}, Org{}, err
		}
		if taken {
			return User{}, Org{}, conflictError{c.msg}
		}
	}
	now := nu.CreatedAt.UTC()
	u := User{
		ID: uuid.NewV4().String(), Username: nu.Username, Email: nu.Email, FullName: nu.FullName, Role: role,
		Status: StatusActive, Provider: nu.Provider, ProviderID: nu.ProviderID, CreatedAt: now, UpdatedAt: now,
	}
	orgSlug, err := freeSlug(ctx, tx, slug(u.Username, u.ID))
	if err != nil {
		return User{}, Org{}, err
	}
	o := Org{ID: uuid.NewV4().String(), Name: u.Username, Slug: orgSlug, OwnerID: u.ID, Status: StatusActive,
		CreatedAt: now, UpdatedAt: now}
	at := now.UnixNano()
	for _, w := range []struct {
		what, stmt string
		args       []any
	}{
		{"user", `INSERT INTO users (id, username, email, full_name, role, status, provider, provider_id, created_at, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, []any{u.ID, u.Username, u.Email, u.FullName, u.Role, u.Status, u.Provider, u.ProviderID, at, at}},
		{"org", `INSERT INTO orgs (id, name, slug, url, owner_id, status, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			[]any{o.ID, o.Name, o.Slug, o.URL, o.OwnerID, o.Status, at, at}},
		{"membership", `INSERT INTO memberships (org_id, user_id, created_at) VALUES (?, ?, ?)`, []any{o.ID, u.ID, at}},
	} {
		if _, err := tx.ExecContext(ctx, w.stmt, w.args...); err != nil {
			// The checks above leave this only to an id drawn twice.
			if isUniqueViolation(err) {
				return User{}, Org{}, conflictError{"the " + w.what + " is taken"}
			}
			return User{}, Org{}, fmt.Errorf("store the %s: %w", w.what, err)
		}
	}
	return u, o, nil
}

// freeSlug returns base, or the first of <base>-1 to <base>-10 that no org
// has as its slug.
func freeSlug(ctx context.Context, tx *sql.Tx, base string) (string, error) {
	for i := 0; i <= slugSuffixes; i++ {
		s := base
		if i > 0 {
			s += "-" + strconv.Itoa(i)
		}
		taken, err := exists(ctx, tx, `SELECT count(*) FROM orgs WHERE slug = ?`, s)
		if err != nil || !taken {
			return s, err
		}
	}
	return "", conflictError{"no free slug is left for the org"}
}

// exists reports whether the count query returns more than zero.
func exists(ctx context.Context, q Querier, query string, args ...any) (bool, error) {
	var n int
	if err := q.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return false, fmt.Errorf("check uniqueness: %w", err)
	}
	return n > 0, nil
}

// isUniqueViolation reports whether err is SQLite refusing a duplicate of a
// unique column or primary key.
func isUniqueViolation(err error) bool {
	var e *sqlite.Error
	return errors.As(err, &e) && (e.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE || e.Code() == sqlite3.SQLITE_CONSTRAINT_PRIMARYKEY)
}
