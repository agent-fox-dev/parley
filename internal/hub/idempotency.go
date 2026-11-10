package hub

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/agent-fox-dev/parley/internal/server"
	"github.com/agent-fox-dev/parley/internal/store"
)

// idempotencyStep creates the table of idempotency records in the
// transactional store, so that a create's answer is replayed across a
// restart for its 24 hours.
var idempotencyStep = store.Step{
	Name:   "server.idempotency_records",
	Target: store.Main,
	Apply: func(ctx context.Context, tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `CREATE TABLE idempotency_records (
	key        TEXT NOT NULL,
	caller     TEXT NOT NULL,
	route      TEXT NOT NULL,
	body_hash  TEXT NOT NULL,
	response   BLOB NOT NULL,
	created_at INTEGER NOT NULL,
	PRIMARY KEY (key, caller, route)
)`)
		return err
	},
}

// idempotencyStore implements the server's record store interface over the
// transactional store.
type idempotencyStore struct{ st *store.Store }

var _ server.IdempotencyStore = idempotencyStore{}

func (s idempotencyStore) Find(ctx context.Context, key, caller, route string) (*server.IdempotencyRecord, error) {
	rec := server.IdempotencyRecord{Key: key, Caller: caller, Route: route}
	var created int64
	err := s.st.DB().QueryRowContext(ctx,
		`SELECT body_hash, response, created_at FROM idempotency_records WHERE key = ? AND caller = ? AND route = ?`,
		key, caller, route).Scan(&rec.BodyHash, &rec.Response, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rec.CreatedAt = time.Unix(0, created).UTC()
	return &rec, nil
}

func (s idempotencyStore) Put(ctx context.Context, rec server.IdempotencyRecord) error {
	return s.st.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT OR REPLACE INTO idempotency_records (key, caller, route, body_hash, response, created_at) VALUES (?, ?, ?, ?, ?, ?)`,
			rec.Key, rec.Caller, rec.Route, rec.BodyHash, rec.Response, rec.CreatedAt.UnixNano())
		return err
	})
}

func (s idempotencyStore) DeleteBefore(ctx context.Context, cutoff time.Time) error {
	return s.st.Tx(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `DELETE FROM idempotency_records WHERE created_at <= ?`, cutoff.UnixNano())
		return err
	})
}
