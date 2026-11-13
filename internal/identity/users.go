package identity

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"
	"uuid"
)

// UserBySelector returns the user the selector sel names, or ErrNotFound.
// The shape of sel decides what it is compared with, and nothing else is
// tried: a value in hyphenated UUID form is an id, any other value with an
// @ an e-mail, and every other value a username. The comparison is exact,
// letter case included.
func UserBySelector(ctx context.Context, q Querier, sel string) (User, error) {
	switch {
	case isUUID(sel):
		return UserByID(ctx, q, sel)
	case strings.Contains(sel, "@"):
		return userWhere(ctx, q, `email = ?`, sel)
	default:
		return userWhere(ctx, q, `username = ?`, sel)
	}
}

// isUUID reports whether s is a UUID in its hyphenated form, the form of
// every id the hub draws.
func isUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	_, err := uuid.Parse(s)
	return err == nil
}

// userPosition is the position of a user in the list of users: newest
// first, created_at descending, then id descending.
type userPosition struct {
	CreatedAt int64  `json:"c"`
	ID        string `json:"id"`
}

// listUsers returns up to limit users in the order of the list of users,
// after the position after when it is not nil, the blocked ones only when
// includeBlocked is true.
func listUsers(ctx context.Context, q RowsQuerier, includeBlocked bool, after *userPosition, limit int) ([]User, error) {
	query := `SELECT id, username, email, full_name, role, status, provider, provider_id, created_at, updated_at
FROM users WHERE 1 = 1`
	var args []any
	if !includeBlocked {
		query += ` AND status = ?`
		args = append(args, StatusActive)
	}
	if after != nil {
		query += ` AND (created_at < ? OR created_at = ? AND id < ?)`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	rows, err := q.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list the users: %w", err)
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		var created, updated int64
		if err := rows.Scan(&u.ID, &u.Username, &u.Email, &u.FullName, &u.Role, &u.Status, &u.Provider,
			&u.ProviderID, &created, &updated); err != nil {
			return nil, fmt.Errorf("list the users: %w", err)
		}
		u.CreatedAt, u.UpdatedAt = fromNanos(created), fromNanos(updated)
		users = append(users, u)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the users: %w", err)
	}
	return users, nil
}

// updateFullName stores fullName, the empty string included, as the
// full_name of the user id in tx, with updated_at now, and returns the
// stored user, or ErrNotFound when there is no such user.
func updateFullName(ctx context.Context, tx *sql.Tx, id, fullName string, now time.Time) (User, error) {
	_, err := tx.ExecContext(ctx, `UPDATE users SET full_name = ?, updated_at = ? WHERE id = ?`,
		fullName, now.UTC().UnixNano(), id)
	if err != nil {
		return User{}, fmt.Errorf("store the full name: %w", err)
	}
	return UserByID(ctx, tx, id)
}
