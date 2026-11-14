package access

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"strings"
)

// RevokeBoundTo revokes, in tx, which it does not commit, every live PAT
// bound to one of slugs, and returns their token ids sorted ascending: a
// non-nil list, empty when no slug is given or none of them has a live
// token. A live token is neither revoked nor expired at the service's
// clock; a revoked token keeps its first revoked_at and an expired one is
// left as it is, so neither is returned. The components that remove
// workspaces (workspace delete, fork expiry) call it in the transaction
// that removes them, so the removal and the revocation commit or roll back
// together, and carry the ids in their own event: it emits none. On a
// failed write it returns the error and a nil list, and the caller rolls
// tx back.
func (s *Service) RevokeBoundTo(ctx context.Context, tx *sql.Tx, slugs []string) ([]string, error) {
	ids := []string{}
	if len(slugs) == 0 {
		return ids, nil
	}
	// revoked_at holds the instant, as identity's revocations write it; the
	// Token object shows it to the whole second.
	at := s.now().UTC().UnixNano()
	args := make([]any, 0, len(slugs)+2)
	args = append(args, at)
	for _, slug := range slugs {
		args = append(args, slug)
	}
	args = append(args, at)
	rows, err := tx.QueryContext(ctx, `UPDATE pats SET revoked_at = ?
WHERE workspace IN (?`+strings.Repeat(", ?", len(slugs)-1)+`)
AND revoked_at IS NULL AND (expires_at IS NULL OR expires_at > ?)
RETURNING token_id`, args...)
	if err != nil {
		return nil, fmt.Errorf("revoke the tokens bound to %d workspaces: %w", len(slugs), err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("revoke the tokens bound to %d workspaces: %w", len(slugs), err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("revoke the tokens bound to %d workspaces: %w", len(slugs), err)
	}
	slices.Sort(ids)
	return ids, nil
}
