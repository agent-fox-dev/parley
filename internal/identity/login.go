package identity

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ErrConflict is a uniqueness conflict of a sign-in or of a user's creation.
var ErrConflict = errors.New("conflict")

// exists reports whether a query finds a row.
func exists(ctx context.Context, q Querier, query string, args ...any) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, query, args...).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

// NewUser are the fields of a user to create.
type NewUser struct {
	Username   string
	Email      string
	FullName   string
	Provider   string
	ProviderID string
}

// CreateUserWithOrg creates, in the open transaction, an active user with
// the given role, the user's personal org (named after the username, owned
// by the user, with the slug the personal-organization rule derives) and
// the user's membership of it. It returns ErrConflict, writing nothing, when
// the username, the e-mail or the provider identity is taken, an org already
// has the username as its name, or no slug is free. The sign-in picks the
// username and the role first; package 05 calls it from POST /users.
func CreateUserWithOrg(ctx context.Context, tx *sql.Tx, nu NewUser, role string, now time.Time) (User, error) {
	checks := []struct {
		query string
		args  []any
	}{
		{`SELECT 1 FROM users WHERE username = ?`, []any{nu.Username}},
		{`SELECT 1 FROM users WHERE email = ?`, []any{nu.Email}},
		{`SELECT 1 FROM orgs WHERE name = ?`, []any{nu.Username}},
	}
	if nu.Provider != "" {
		checks = append(checks, struct {
			query string
			args  []any
		}{`SELECT 1 FROM users WHERE provider = ? AND provider_id = ?`, []any{nu.Provider, nu.ProviderID}})
	}
	for _, c := range checks {
		taken, err := exists(ctx, tx, c.query, c.args...)
		if err != nil {
			return User{}, err
		}
		if taken {
			return User{}, ErrConflict
		}
	}
	u, err := CreateUser(ctx, tx, User{Username: nu.Username, Email: nu.Email, FullName: nu.FullName, Role: role,
		Status: StatusActive, Provider: nu.Provider, ProviderID: nu.ProviderID}, now)
	if err != nil {
		return User{}, err
	}
	slug, err := freeSlug(ctx, tx, baseSlug(u.Username, u.ID))
	if err != nil {
		return User{}, err
	}
	orgID := NewID()
	if _, err := tx.ExecContext(ctx, `INSERT INTO orgs (id, name, slug, owner_id, status, created_at, updated_at) VALUES (?, ?, ?, ?, 'active', ?, ?)`,
		orgID, u.Username, slug, u.ID, nanos(now), nanos(now)); err != nil {
		return User{}, err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO memberships (org_id, user_id, created_at) VALUES (?, ?, ?)`, orgID, u.ID, nanos(now)); err != nil {
		return User{}, err
	}
	return u, nil
}

// freeUsername returns the username, or the first free of <name>-2 to
// <name>-10; ErrConflict when none is free.
func freeUsername(ctx context.Context, q Querier, name string) (string, error) {
	candidates := []string{name}
	for i := 2; i <= 10; i++ {
		candidates = append(candidates, fmt.Sprintf("%s-%d", name, i))
	}
	for _, c := range candidates {
		taken, err := exists(ctx, q, `SELECT 1 FROM users WHERE username = ?`, c)
		if err != nil {
			return "", err
		}
		if !taken {
			return c, nil
		}
	}
	return "", ErrConflict
}

// newUserRole is admin when the e-mail equals the bootstrap admin_email,
// ignoring letter case, and no user with role admin exists in any status.
func newUserRole(ctx context.Context, tx *sql.Tx, email string) (string, error) {
	var adminEmail string
	err := tx.QueryRowContext(ctx, `SELECT email FROM admin_settings WHERE id = 1`).Scan(&adminEmail)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return "", err
	}
	if adminEmail == "" || !strings.EqualFold(adminEmail, email) {
		return RoleUser, nil
	}
	hasAdmin, err := exists(ctx, tx, `SELECT 1 FROM users WHERE role = ?`, RoleAdmin)
	if err != nil {
		return "", err
	}
	if hasAdmin {
		return RoleUser, nil
	}
	return RoleAdmin, nil
}

// signedIn is the result of a sign-in.
type signedIn struct {
	user      User
	key       string
	keyID     string
	expiresAt *time.Time
}

// errBlocked refuses the login of a blocked user.
var errBlocked = errors.New("user is blocked")

// signIn runs in one transaction: it finds the user by provider identity
// and refreshes it, or creates the user with the personal org; then it
// revokes every live API key of the user and issues one of days.
func signIn(ctx context.Context, tx *sql.Tx, id Identity, days int, now time.Time) (signedIn, error) {
	user, err := scanUser(tx.QueryRowContext(ctx, `SELECT `+userColumns+` FROM users WHERE provider = ? AND provider_id = ?`, id.Provider, id.ProviderID))
	switch {
	case errors.Is(err, ErrNotFound):
		user, err = createSignedInUser(ctx, tx, id, now)
	case err == nil:
		user, err = refreshUser(ctx, tx, user, id, now)
	}
	if err != nil {
		return signedIn{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE api_keys SET revoked_at = ?
WHERE user_id = ? AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)`, nanos(now), user.ID, nanos(now)); err != nil {
		return signedIn{}, err
	}
	key, keyID, expiresAt, err := IssueAPIKey(ctx, tx, user.ID, days, now)
	if err != nil {
		return signedIn{}, err
	}
	return signedIn{user: user, key: key, keyID: keyID, expiresAt: expiresAt}, nil
}

// refreshUser updates an existing user's e-mail and username from the
// provider: a blocked user is refused; an e-mail of another user is a
// conflict; a username of another user keeps the old one. Id, role,
// full_name, created_at and the personal org stay as they are.
func refreshUser(ctx context.Context, tx *sql.Tx, user User, id Identity, now time.Time) (User, error) {
	if user.Status == StatusBlocked {
		return User{}, errBlocked
	}
	emailTaken, err := exists(ctx, tx, `SELECT 1 FROM users WHERE email = ? AND id != ?`, id.Email, user.ID)
	if err != nil {
		return User{}, err
	}
	if emailTaken {
		return User{}, ErrConflict
	}
	nameTaken, err := exists(ctx, tx, `SELECT 1 FROM users WHERE username = ? AND id != ?`, id.Username, user.ID)
	if err != nil {
		return User{}, err
	}
	username := id.Username
	if nameTaken {
		username = user.Username
	}
	if _, err := tx.ExecContext(ctx, `UPDATE users SET email = ?, username = ?, updated_at = ? WHERE id = ?`,
		id.Email, username, nanos(now), user.ID); err != nil {
		return User{}, err
	}
	return GetUser(ctx, tx, user.ID)
}

// createSignedInUser creates the user a first login brings.
func createSignedInUser(ctx context.Context, tx *sql.Tx, id Identity, now time.Time) (User, error) {
	username, err := freeUsername(ctx, tx, id.Username)
	if err != nil {
		return User{}, err
	}
	role, err := newUserRole(ctx, tx, id.Email)
	if err != nil {
		return User{}, err
	}
	return CreateUserWithOrg(ctx, tx, NewUser{Username: username, Email: id.Email, FullName: id.FullName,
		Provider: id.Provider, ProviderID: id.ProviderID}, role, now)
}
