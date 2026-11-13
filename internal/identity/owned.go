package identity

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
)

// owned is a credential a user owns, an API key or a PAT: what a lookup by
// owner and a list of one user's credentials read of it.
type owned interface {
	ownership() (id, userID string, createdAt time.Time)
}

func (k APIKey) ownership() (string, string, time.Time) { return k.KeyID, k.UserID, k.CreatedAt }

func (p PAT) ownership() (string, string, time.Time) { return p.TokenID, p.UserID, p.CreatedAt }

// ownedTable is the table of one kind of credential users own, so that
// keys and tokens share one lookup by owner and one keyset-paged list.
type ownedTable[T owned] struct {
	table    string // the table's name
	idColumn string // the column of the credential's id
	columns  string // the columns scan reads, in its order
	plural   string // what a list error calls the credentials
	scan     func(row interface{ Scan(dest ...any) error }) (T, error)
	byID     func(ctx context.Context, q Querier, id string) (T, error)
	notFound func() error
}

var (
	keyTable = ownedTable[APIKey]{
		table: "api_keys", idColumn: "key_id", columns: apiKeyColumns, plural: "API keys",
		scan: scanAPIKey, byID: APIKeyByID, notFound: errKeyNotFound,
	}
	tokenTable = ownedTable[PAT]{
		table: "pats", idColumn: "token_id", columns: patColumns, plural: "PATs",
		scan: scanPAT, byID: PATByID, notFound: errTokenNotFound,
	}
)

// ofUser returns the credential id when the user userID owns it. One that
// does not exist and one of another user are both the table's notFound
// answer, so that the answer tells nothing of other users' credentials.
func (t ownedTable[T]) ofUser(ctx context.Context, q Querier, userID, id string) (T, error) {
	v, err := t.byID(ctx, q, id)
	if err == nil {
		if _, owner, _ := v.ownership(); owner != userID {
			err = ErrNotFound
		}
	}
	if errors.Is(err, ErrNotFound) {
		var zero T
		return zero, t.notFound()
	}
	return v, err
}

// ownedPosition is the position of a credential in a list of one user's
// credentials: newest first, created_at descending, then the id
// descending, so that credentials created at the same instant keep one
// order across pages.
type ownedPosition struct {
	CreatedAt int64  `json:"c"`
	ID        string `json:"id"`
}

// position returns the position of v in a list of credentials.
func position[T owned](v T) any {
	id, _, created := v.ownership()
	return ownedPosition{CreatedAt: created.UnixNano(), ID: id}
}

// writePage answers a page of the credentials of the user userID in any
// state, in the order of a list of credentials, read through q.
func (t ownedTable[T]) writePage(w http.ResponseWriter, r *http.Request, q RowsQuerier, userID string) error {
	page, err := server.ReadPage(r)
	if err != nil {
		return err
	}
	after, err := pageAfter[ownedPosition](page)
	if err != nil {
		return err
	}
	items, err := t.list(r.Context(), q, userID, after, page.Limit+1)
	if err != nil {
		return err
	}
	return server.WritePage(w, page, items, position[T])
}

// list returns up to limit credentials of the user userID in any state, in
// the order of a list of credentials, after the position after when it is
// not nil.
func (t ownedTable[T]) list(ctx context.Context, q RowsQuerier, userID string, after *ownedPosition, limit int) ([]T, error) {
	query := `SELECT ` + t.columns + ` FROM ` + t.table + ` WHERE user_id = ?`
	args := []any{userID}
	if after != nil {
		query += ` AND (created_at < ? OR created_at = ? AND ` + t.idColumn + ` < ?)`
		args = append(args, after.CreatedAt, after.CreatedAt, after.ID)
	}
	query += ` ORDER BY created_at DESC, ` + t.idColumn + ` DESC LIMIT ?`
	rows, err := q.QueryContext(ctx, query, append(args, limit)...)
	if err != nil {
		return nil, fmt.Errorf("list the %s: %w", t.plural, err)
	}
	defer rows.Close()
	var items []T
	for rows.Next() {
		v, err := t.scan(rows)
		if err != nil {
			return nil, fmt.Errorf("list the %s: %w", t.plural, err)
		}
		items = append(items, v)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list the %s: %w", t.plural, err)
	}
	return items, nil
}
