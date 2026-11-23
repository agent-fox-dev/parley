package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// Nanos is *t as a query argument in nanoseconds since the epoch, NULL for
// nil.
func Nanos(t *time.Time) sql.NullInt64 {
	if t == nil {
		return sql.NullInt64{}
	}
	return sql.NullInt64{Int64: t.UnixNano(), Valid: true}
}

// TimeOf is a nullable column of nanoseconds since the epoch as a UTC time,
// nil for NULL.
func TimeOf(n sql.NullInt64) *time.Time {
	if !n.Valid {
		return nil
	}
	t := time.Unix(0, n.Int64).UTC()
	return &t
}

// Args is ss as query arguments.
func Args(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// ReadTx runs fn in a read-only transaction of db, which sees one snapshot
// of the store. A read-only transaction begins deferred, not with the write
// lock the store's other transactions take (_txlock=immediate), so that it
// neither waits for a writer nor holds one up.
func ReadTx(ctx context.Context, db *sql.DB, fn func(*sql.Tx) error) error {
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return fmt.Errorf("begin a read: %w", err)
	}
	defer tx.Rollback()
	return fn(tx)
}
