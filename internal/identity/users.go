package identity

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

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
